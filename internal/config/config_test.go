package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const bothChannels = `{
  "version": 1,
  "channels": {
    "alerts":  {"type":"telegram","token":"123:abc"},
    "notices": {"type":"email","host":"smtp.example.com"}
  }
}`

func TestDefaultsAreApplied(t *testing.T) {
	cfg, err := Load(write(t, bothChannels))
	if err != nil {
		t.Fatal(err)
	}
	tg := cfg.Channels["alerts"]
	if tg.APIBase != DefaultAPIBase {
		t.Errorf("api_base = %q, want %q", tg.APIBase, DefaultAPIBase)
	}
	if tg.MaxBody != DefaultMaxBody {
		t.Errorf("max_body = %v, want %v", tg.MaxBody, Bytes(DefaultMaxBody))
	}
	if tg.SendTimeout.D() != DefaultSendTimeout {
		t.Errorf("send_timeout = %v, want %v", tg.SendTimeout, DefaultSendTimeout)
	}
	em := cfg.Channels["notices"]
	if em.Port != DefaultSMTPPort || em.Encryption != EncStartTLS {
		t.Errorf("email defaults = %d/%q", em.Port, em.Encryption)
	}
}

// A pinned destination is written either way, because a config file is hand-written and one
// recipient is the common case.
func TestPinnedAcceptsOneOrMany(t *testing.T) {
	cfg, err := Load(write(t, `{"version":1,"channels":{
	  "one":{"type":"email","host":"h","pinned":{"to":"ops@x.com","from":"n@x.com"}},
	  "many":{"type":"email","host":"h","pinned":{"to":["ops@x.com","oncall@x.com"]}},
	  "open":{"type":"email","host":"h"},
	  "tg":{"type":"telegram","token":"1:x","pinned":{"to":"-100123"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Channels["one"].Pinned.To; len(got) != 1 || got[0] != "ops@x.com" {
		t.Errorf("one = %v", got)
	}
	if got := cfg.Channels["many"].Pinned.To; len(got) != 2 {
		t.Errorf("many = %v", got)
	}
	if cfg.Channels["open"].Pinned.Any() {
		t.Error("a channel with no pinned block reports pins")
	}
	if got := cfg.Channels["tg"].Pinned.To; len(got) != 1 || got[0] != "-100123" {
		t.Errorf("tg = %v", got)
	}
}

func TestPerChannelOverrides(t *testing.T) {
	cfg, err := Load(write(t, `{"version":1,"channels":{
      "a":{"type":"telegram","token":"1:x","max_body":"50MiB","send_timeout":"90s","api_base":"http://relay.internal"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ch := cfg.Channels["a"]
	if ch.MaxBody != 50<<20 {
		t.Errorf("max_body = %v", ch.MaxBody)
	}
	if ch.SendTimeout.D() != 90*time.Second {
		t.Errorf("send_timeout = %v", ch.SendTimeout)
	}
	if ch.APIBase != "http://relay.internal" {
		t.Errorf("api_base = %q", ch.APIBase)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no channels", `{"version":1,"channels":{}}`, "no channels"},
		{"wrong version", `{"version":2,"channels":{"a":{"type":"telegram","token":"1:x"}}}`, "version 2"},
		{"unknown top key", `{"version":1,"log_level":"info","channels":{}}`, "unknown field"},
		{"bad type", `{"version":1,"channels":{"a":{"type":"sms"}}}`, "not telegram or email"},
		{"no type", `{"version":1,"channels":{"a":{"host":"x"}}}`, `no "type"`},
		{"telegram key on email", `{"version":1,"channels":{"a":{"type":"email","host":"h","api_base":"http://x"}}}`, "unknown field"},
		{"email key on telegram", `{"version":1,"channels":{"a":{"type":"telegram","token":"1:x","host":"h"}}}`, "unknown field"},
		{"a pinnable field outside the block", `{"version":1,"channels":{"a":{"type":"email","host":"h","from":"a@x.com"}}}`, "unknown field"},
		{"unknown key inside pinned", `{"version":1,"channels":{"a":{"type":"email","host":"h","pinned":{"body":"b"}}}}`, "unknown field"},
		{"subject inside pinned", `{"version":1,"channels":{"a":{"type":"email","host":"h","pinned":{"subject":"s"}}}}`, "unknown field"},
		{"from pinned on telegram", `{"version":1,"channels":{"a":{"type":"telegram","token":"1:x","pinned":{"from":"a@x.com"}}}}`, "unknown field"},
		{"bad pinned from", `{"version":1,"channels":{"a":{"type":"email","host":"h","pinned":{"from":"not an address"}}}}`, "pinned"},
		{"bad pinned to", `{"version":1,"channels":{"a":{"type":"email","host":"h","pinned":{"to":"not an address"}}}}`, "pinned"},
		{"line break in a pinned address", `{"version":1,"channels":{"a":{"type":"email","host":"h","pinned":{"to":"a@x.com\r\nBcc: e@x.com"}}}}`, "line break"},
		{"empty pinned to", `{"version":1,"channels":{"a":{"type":"email","host":"h","pinned":{"to":""}}}}`, "empty recipient"},
		{"two pinned chats", `{"version":1,"channels":{"a":{"type":"telegram","token":"1:x","pinned":{"to":["-1","-2"]}}}}`, "one chat"},
		{"misspelled key", `{"version":1,"channels":{"a":{"type":"email","host":"h","passwrod":"x"}}}`, "unknown field"},
		{"bad channel name", `{"version":1,"channels":{"Alerts!":{"type":"telegram","token":"1:x"}}}`, "a name is lowercase"},
		{"bot prefix on token", `{"version":1,"channels":{"a":{"type":"telegram","token":"bot123:abc"}}}`, "shaped <digits>"},
		{"no host", `{"version":1,"channels":{"a":{"type":"email"}}}`, "no host"},
		{"bad encryption", `{"version":1,"channels":{"a":{"type":"email","host":"h","encryption":"ssl"}}}`, "not starttls"},
		{"none with password", `{"version":1,"channels":{"a":{"type":"email","host":"h","encryption":"none","password":"p"}}}`, "cannot carry user or password"},
		{"bad port", `{"version":1,"channels":{"a":{"type":"email","host":"h","port":70000}}}`, "outside 1-65535"},
		{"bad api_base", `{"version":1,"channels":{"a":{"type":"telegram","token":"1:x","api_base":"nope"}}}`, "absolute URL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.body))
			if err == nil {
				t.Fatalf("accepted %s", c.body)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// A password is the bytes in the file. Nothing is interpolated.
func TestValuesAreLiteral(t *testing.T) {
	const pw = `${SMTP_PASSWORD}$$literal$`
	t.Setenv("SMTP_PASSWORD", "should-not-be-used")
	cfg, err := Load(write(t, `{"version":1,"channels":{"a":{"type":"email","host":"h","user":"u","password":`+quote(pw)+`}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Channels["a"].Password; got != pw {
		t.Errorf("password = %q, want %q verbatim", got, pw)
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// A %v written in a hurry must not put a credential in a log line.
func TestRedaction(t *testing.T) {
	cfg, err := Load(write(t, `{"version":1,"channels":{"a":{"type":"email","host":"h","password":"hunter2"},
      "b":{"type":"telegram","token":"123:SECRETTOKEN"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		cfg.String(),
		cfg.Channels["a"].String(),
		cfg.Channels["b"].String(),
		strings.Join([]string{format(cfg.Channels["a"]), format(cfg.Channels["b"])}, " "),
	} {
		for _, secret := range []string{"hunter2", "SECRETTOKEN"} {
			if strings.Contains(s, secret) {
				t.Errorf("%q leaked %q", s, secret)
			}
		}
	}
}

func format(v any) string { return strings.TrimSpace(sprintf("%v %+v %#v", v, v, v)) }

func TestReloadPicksUpAChannel(t *testing.T) {
	p := write(t, bothChannels)
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := st.Get(); len(cfg.Channels) != 2 {
		t.Fatalf("started with %d channels", len(cfg.Channels))
	}

	rewrite(t, p, `{"version":1,"channels":{
      "alerts":{"type":"telegram","token":"123:abc"},
      "notices":{"type":"email","host":"smtp.example.com"},
      "third":{"type":"email","host":"other.example.com"}}}`)
	cfg, err := st.Get()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != 3 {
		t.Fatalf("after edit: %d channels, want 3", len(cfg.Channels))
	}
}

// A typo must not take down a running notifier.
func TestBrokenReloadKeepsTheOldChannels(t *testing.T) {
	p := write(t, bothChannels)
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, p, `{"version":1,"channels":{`)

	cfg, err := st.Get()
	if err == nil {
		t.Fatal("a broken file loaded cleanly")
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("kept %d channels, want the previous 2", len(cfg.Channels))
	}
	if _, ok := cfg.Channels["alerts"]; !ok {
		t.Error("the previous channels are gone")
	}
}

func rewrite(t *testing.T, path, body string) {
	t.Helper()
	// Move the mtime, which is what the store watches.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

func TestRequireDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := RequireDataDir(dir); err != nil {
		t.Fatalf("a writable dir was refused: %v", err)
	}
	if err := RequireDataDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing dir was accepted")
	}
	file := filepath.Join(dir, "afile")
	os.WriteFile(file, nil, 0o600)
	if err := RequireDataDir(file); err == nil {
		t.Error("a file was accepted as the data directory")
	}

	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o500)
	if os.Getuid() != 0 {
		if err := RequireDataDir(ro); err == nil {
			t.Error("a read-only dir was accepted")
		}
	}
}

func TestEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		e, err := LoadEnv()
		if err != nil {
			t.Fatal(err)
		}
		if e.ConfigPath != DefaultConfigPath || e.DataDir != DefaultDataDir {
			t.Errorf("paths = %q %q", e.ConfigPath, e.DataDir)
		}
		if e.Port != "" {
			t.Errorf("PORT defaulted to %q; the image sets the port, not the environment", e.Port)
		}
	})
	t.Run("bad level", func(t *testing.T) {
		t.Setenv(LogLevelEnv, "verbose")
		if _, err := LoadEnv(); err == nil {
			t.Error("a bad log level was accepted")
		}
	})
	t.Run("bad backup url", func(t *testing.T) {
		t.Setenv(BackupURLEnv, "backup:8080/backup")
		if _, err := LoadEnv(); err == nil {
			t.Error("a relative backup URL was accepted")
		}
	})
}

// The sidecar holds the token, the provider and the subdirectory, so one URL is the whole of
// notifio's backup configuration.
func TestBackupIsOneSetting(t *testing.T) {
	t.Setenv(BackupURLEnv, "http://backup:8080/backup")
	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.BackupURL != "http://backup:8080/backup" {
		t.Errorf("BackupURL = %q", e.BackupURL)
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]Bytes{"25MiB": 25 << 20, "1KiB": 1 << 10, "10MB": 10_000_000, "512": 512, "2G": 2 << 30}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseBytes("lots"); err == nil {
		t.Error("accepted a size of 'lots'")
	}
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
