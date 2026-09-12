// Package config is the environment notifio was started with and the channels it was given.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Environment variables. Only LogLevelEnv and BackupURLEnv are part of the image's surface;
// the other three exist for tests and local runs.
const (
	LogLevelEnv  = "NOTIFIO_LOG_LEVEL"
	BackupURLEnv = "NOTIFIO_BACKUP_URL"
	ConfigEnv    = "NOTIFIO_CONFIG"
	DataDirEnv   = "NOTIFIO_DATA_DIR"
	PortEnv      = "PORT"
)

// Defaults for the two paths and for every per-channel setting.
const (
	DefaultConfigPath  = "/data/config.json"
	DefaultDataDir     = "/data"
	DefaultMaxBody     = 25 << 20
	DefaultSendTimeout = 60 * time.Second
	DefaultAPIBase     = "https://api.telegram.org"
	DefaultSMTPPort    = 587
)

// FileVersion is the only config format this build understands.
const FileVersion = 1

// Channel types.
const (
	TypeTelegram = "telegram"
	TypeEmail    = "email"
)

// SMTP encryption modes.
const (
	EncStartTLS = "starttls"
	EncTLS      = "tls"
	EncNone     = "none"
)

// nameRe is what a channel may be called. Channel names reach log lines and the command line,
// so they stay greppable.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// botTokenRe catches a bot token pasted with its "bot" method prefix still attached.
var botTokenRe = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// Env is what the process was started with.
type Env struct {
	LogLevel   slog.Level
	ConfigPath string
	DataDir    string
	Port       string

	// BackupURL is a backio-agent's POST /backup. The agent holds the credential and decides
	// where archives land, so this is the whole of notifio's backup configuration.
	BackupURL string
}

// Channel is one configured destination. Fields not belonging to its Type are absent rather
// than empty: an unknown key for a type is a load error.
type Channel struct {
	Name string `json:"-"`
	Type string `json:"type"`

	MaxBody     Bytes    `json:"max_body,omitempty"`
	SendTimeout Duration `json:"send_timeout,omitempty"`

	// Pinned holds request fields this channel fixes. A field set here cannot be sent.
	Pinned Pinned `json:"pinned,omitempty"`

	// telegram
	Token   string `json:"token,omitempty"`
	APIBase string `json:"api_base,omitempty"`

	// email
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Encryption string `json:"encryption,omitempty"`
	User       string `json:"user,omitempty"`
	Password   string `json:"password,omitempty"`
}

// Pinned is the part of a message the config decides.
//
// Every key is the name of a request field. Set one and a request carrying it is refused; leave
// it out and the request must supply it. That is the whole rule, and it is why these live in a
// block of their own rather than beside the host and the credentials.
type Pinned struct {
	// To is the destination: a chat id for telegram, one or more addresses for email.
	To Recipients `json:"to,omitempty"`
	// From is the sender, on an email channel.
	From string `json:"from,omitempty"`
	// LinkPreview is whether Telegram renders a preview card for a link in the body. A
	// pointer because unset and false are different: unset leaves it to the request.
	LinkPreview *bool `json:"link_preview,omitempty"`
}

// Any reports whether the channel pins anything.
func (p Pinned) Any() bool { return len(p.To) > 0 || p.From != "" || p.LinkPreview != nil }

// String redacts. Without it a %v written in a hurry puts a bot token in a log line.
func (c Channel) String() string {
	return fmt.Sprintf("Channel{Name:%s Type:%s}", c.Name, c.Type)
}

// GoString redacts under %#v too.
func (c Channel) GoString() string { return c.String() }

// File is the on-disk shape.
type File struct {
	Version  int                 `json:"version"`
	Channels map[string]*Channel `json:"channels"`
}

// Config is one loaded config file.
type Config struct {
	Channels map[string]*Channel
}

// Names returns the channel names, sorted, for an error message a person reads.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Channels))
	for n := range c.Channels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// String redacts, for the same reason [Channel.String] does.
func (c *Config) String() string {
	return fmt.Sprintf("Config{Channels:%v}", c.Names())
}

// LoadEnv reads the environment. A bad value here is a startup error, so a mistyped log level
// is reported rather than silently becoming info.
func LoadEnv() (*Env, error) {
	env := &Env{
		LogLevel:   slog.LevelInfo,
		ConfigPath: getenv(ConfigEnv, DefaultConfigPath),
		DataDir:    getenv(DataDirEnv, DefaultDataDir),
		Port:       getenv(PortEnv, ""),
	}

	if v := strings.TrimSpace(os.Getenv(LogLevelEnv)); v != "" {
		switch strings.ToLower(v) {
		case "debug":
			env.LogLevel = slog.LevelDebug
		case "info":
			env.LogLevel = slog.LevelInfo
		case "warn", "warning":
			env.LogLevel = slog.LevelWarn
		case "error":
			env.LogLevel = slog.LevelError
		default:
			return nil, fmt.Errorf("%s: %q is not one of debug, info, warn, error", LogLevelEnv, v)
		}
	}

	if v := strings.TrimSpace(os.Getenv(BackupURLEnv)); v != "" {
		if err := validateBackupURL(v); err != nil {
			return nil, fmt.Errorf("%s: %w", BackupURLEnv, err)
		}
		env.BackupURL = v
	}
	return env, nil
}

// validateBackupURL checks only what notifio can honestly check. Whether the agent accepts the
// request is the agent's answer, and the first push is immediate.
func validateBackupURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if !u.IsAbs() {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

// RequireDataDir proves the volume is there and writable before anything needs it.
func RequireDataDir(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s does not exist: mount a volume at %s", dir, dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	probe := filepath.Join(dir, ".notifio-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	f.Close()
	return os.Remove(probe)
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist: write it before starting — see docs/channels.md", path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f, path)
}

func parse(r io.Reader, path string) (*Config, error) {
	// Two passes: the first reads each channel's type, the second decodes it into that type's
	// keys with DisallowUnknownFields, so a telegram key on an email channel is an error
	// rather than something silently dropped.
	var raw struct {
		Version  int                        `json:"version"`
		Channels map[string]json.RawMessage `json:"channels"`
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if raw.Version != FileVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", path, raw.Version, FileVersion)
	}
	if len(raw.Channels) == 0 {
		return nil, fmt.Errorf("%s: no channels; there is nothing a token could be issued for", path)
	}

	cfg := &Config{Channels: make(map[string]*Channel, len(raw.Channels))}
	for name, body := range raw.Channels {
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("%s: channel %q: a name is lowercase letters, digits, _ and -, up to 64", path, name)
		}
		ch, err := decodeChannel(name, body)
		if err != nil {
			return nil, fmt.Errorf("%s: channel %q: %w", path, name, err)
		}
		cfg.Channels[name] = ch
	}
	return cfg, nil
}

func decodeChannel(name string, body json.RawMessage) (*Channel, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}

	// Decoding into a per-type struct is what makes an out-of-place key an unknown field.
	var ch Channel
	switch probe.Type {
	case TypeTelegram:
		var t struct {
			Type        string   `json:"type"`
			MaxBody     Bytes    `json:"max_body"`
			SendTimeout Duration `json:"send_timeout"`
			Token       string   `json:"token"`
			APIBase     string   `json:"api_base"`
			// Only `to` — a telegram message has no sender to choose.
			Pinned struct {
				To          Recipients `json:"to"`
				LinkPreview *bool      `json:"link_preview"`
			} `json:"pinned"`
		}
		if err := strictUnmarshal(body, &t); err != nil {
			return nil, err
		}
		ch = Channel{
			Type: t.Type, MaxBody: t.MaxBody, SendTimeout: t.SendTimeout,
			Token: t.Token, APIBase: t.APIBase,
			Pinned: Pinned{To: t.Pinned.To, LinkPreview: t.Pinned.LinkPreview},
		}
	case TypeEmail:
		var e struct {
			Type        string   `json:"type"`
			MaxBody     Bytes    `json:"max_body"`
			SendTimeout Duration `json:"send_timeout"`
			Host        string   `json:"host"`
			Port        int      `json:"port"`
			Encryption  string   `json:"encryption"`
			User        string   `json:"user"`
			Password    string   `json:"password"`
			// No link_preview: an email client renders what the HTML says.
			// No subject either: it is the message, like the body, and not a property of the
			// channel.
			Pinned struct {
				To   Recipients `json:"to"`
				From string     `json:"from"`
			} `json:"pinned"`
		}
		if err := strictUnmarshal(body, &e); err != nil {
			return nil, err
		}
		ch = Channel{
			Type: e.Type, MaxBody: e.MaxBody, SendTimeout: e.SendTimeout,
			Host: e.Host, Port: e.Port, Encryption: e.Encryption,
			User: e.User, Password: e.Password,
			Pinned: Pinned{To: e.Pinned.To, From: e.Pinned.From},
		}
	case "":
		return nil, errors.New(`no "type"`)
	default:
		return nil, fmt.Errorf("type %q is not telegram or email", probe.Type)
	}

	ch.Name = name
	applyDefaults(&ch)
	return &ch, validate(&ch)
}

func strictUnmarshal(body json.RawMessage, into any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func applyDefaults(ch *Channel) {
	if ch.MaxBody == 0 {
		ch.MaxBody = DefaultMaxBody
	}
	if ch.SendTimeout == 0 {
		ch.SendTimeout = Duration(DefaultSendTimeout)
	}
	switch ch.Type {
	case TypeTelegram:
		if ch.APIBase == "" {
			ch.APIBase = DefaultAPIBase
		}
	case TypeEmail:
		if ch.Port == 0 {
			ch.Port = DefaultSMTPPort
		}
		if ch.Encryption == "" {
			ch.Encryption = EncStartTLS
		}
	}
}

func validate(ch *Channel) error {
	if ch.MaxBody <= 0 {
		return errors.New("max_body must be positive")
	}
	if ch.SendTimeout <= 0 {
		return errors.New("send_timeout must be positive")
	}

	switch ch.Type {
	case TypeTelegram:
		if strings.TrimSpace(ch.Token) == "" {
			return errors.New("no token")
		}
		if !botTokenRe.MatchString(ch.Token) {
			return errors.New(`token is not shaped <digits>:<rest> — a leading "bot" is part of the URL, not the token`)
		}
		if err := validateBaseURL(ch.APIBase); err != nil {
			return fmt.Errorf("api_base: %w", err)
		}
		if len(ch.Pinned.To) > 1 {
			return fmt.Errorf("pinned.to takes one chat, got %d", len(ch.Pinned.To))
		}

	case TypeEmail:
		if strings.TrimSpace(ch.Host) == "" {
			return errors.New("no host")
		}
		if ch.Port < 1 || ch.Port > 65535 {
			return fmt.Errorf("port %d is outside 1-65535", ch.Port)
		}
		switch ch.Encryption {
		case EncStartTLS, EncTLS:
		case EncNone:
			// Go's SMTP client refuses PLAIN over an unencrypted connection anyway. Refusing
			// here names the reason rather than producing a send that fails mysteriously.
			if ch.User != "" || ch.Password != "" {
				return errors.New(`encryption "none" cannot carry user or password`)
			}
		default:
			return fmt.Errorf("encryption %q is not starttls, tls or none", ch.Encryption)
		}
		// A pinned address has to be good at startup: a caller cannot correct one.
		pinnedAddrs := append([]string{}, ch.Pinned.To...)
		if ch.Pinned.From != "" {
			pinnedAddrs = append(pinnedAddrs, ch.Pinned.From)
		}
		for _, a := range pinnedAddrs {
			if strings.ContainsAny(a, "\r\n") {
				return fmt.Errorf("pinned %q contains a line break", a)
			}
			if _, err := mail.ParseAddress(a); err != nil {
				return fmt.Errorf("pinned %q: %w", a, err)
			}
		}
	}
	return nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	return nil
}

// Store is the config file and a copy of it held in memory.
type Store struct {
	path string

	mu      sync.Mutex
	cfg     *Config
	loadErr error
	modTime time.Time
	size    int64
}

// Open loads the file once. A file that is missing or invalid at startup is fatal.
func Open(path string) (*Store, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	st := &Store{path: path, cfg: cfg}
	if info, err := os.Stat(path); err == nil {
		st.modTime, st.size = info.ModTime(), info.Size()
	}
	return st, nil
}

// Get returns the config in use, re-reading the file when it has changed.
//
// A reload that fails validation keeps what is already loaded: a typo must not take down a
// running notifier. **The returned config is always the one in use, error or not** — a caller
// that drops it on an error is dropping the channels it is still serving.
//
// The error is sticky until a good load replaces it. Without that it would be delivered once,
// to whichever caller happened to be first, and every caller after that would see a clean
// result over a file that is still broken.
func (s *Store) Get() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.path)
	if err != nil {
		// The file is gone or unreadable. Keep serving, and keep saying so.
		s.loadErr = err
		return s.cfg, err
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return s.cfg, s.loadErr
	}
	// Record the stat first, so a file that is broken is not re-read on every request.
	s.modTime, s.size = info.ModTime(), info.Size()

	cfg, err := Load(s.path)
	if err != nil {
		s.loadErr = err
		return s.cfg, err
	}
	s.cfg, s.loadErr = cfg, nil
	return cfg, nil
}

// Path is where this store reads from.
func (s *Store) Path() string { return s.path }

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
