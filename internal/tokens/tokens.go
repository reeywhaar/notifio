// Package tokens is the list of who may send, and where each of them may send.
//
// A token is minted for exactly one channel. See docs/tokens.md.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors a caller needs to tell apart from a broken disk.
var (
	ErrNotFound = errors.New("no such token")
	ErrConflict = errors.New("a token with that label already exists")
	ErrInvalid  = errors.New("invalid label")
)

const (
	// FileName is the file inside the data directory.
	FileName = "data.json"

	// Prefix marks a bearer secret in a log somebody is grepping.
	Prefix = "nt_"

	// SecretPrefix marks the secret of a nonced token. Distinct from [Prefix] because the two
	// are stored differently and cannot be used interchangeably: this one lives in data.json
	// in the clear, and is refused if presented as a bearer token.
	SecretPrefix = "nts_"

	// NoncedPrefix marks what a nonced token puts on the wire, which is not a secret at all:
	// ntc_<unix seconds>.<token id>.<sha256 hex>.
	NoncedPrefix = "ntc_"

	// Sep separates the fields. A dot rather than a colon: it is unreserved in a URL, where a
	// colon is a delimiter, so the value survives being put somewhere it was not meant to go.
	// No field can contain one — they are digits and hex.
	Sep = "."

	// Window is how far a nonce may be from now, in either direction, to allow for clock
	// skew. Five minutes is what Stripe and Slack settled on.
	Window = 5 * time.Minute

	// secretBytes is 256 bits, which is why the hash below can be a fast one.
	secretBytes = 32

	// fileVersion is written into the file so a format change is a migration.
	//
	// Version 2 adds `secret` to a row. A file is written as 1 until it actually holds a
	// nonced token, so an instance that never uses one stays readable by an older binary —
	// and one that does becomes unreadable by it, deliberately: an older binary would ignore
	// `secret`, fall back to `hash`, and accept the secret as a bearer token, which is the
	// exact thing being prevented.
	fileVersion       = 2
	fileVersionBearer = 1

	labelMax = 64

	// IDLen is how much of the hash names a token. Eight hex characters is 32 bits, which is
	// short enough to type and long enough that [Store.Create] re-minting on a clash makes a
	// collision impossible rather than merely unlikely.
	IDLen = 8
)

// Token is one entry: what it is called, where it may send, and enough to recognise it.
type Token struct {
	Label   string `json:"label"`
	Channel string `json:"channel"`
	Hash    string `json:"hash"`

	// Secret is set only on a nonced token, and is the plaintext. A SHA-256 cannot be
	// verified from a digest of itself, so the server has to keep the thing it is checking
	// against. See docs/tokens.md.
	Secret    string `json:"secret,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// Nonced reports whether this token authenticates by nonce rather than as a bearer.
func (t Token) Nonced() bool { return t.Secret != "" }

// Created is CreatedAt as a time, in UTC like everything else here.
func (t Token) Created() time.Time { return time.Unix(t.CreatedAt, 0).UTC() }

// ID names this token without naming its secret or its label.
//
// Derived from the hash rather than stored, so every file that already exists has one and
// nothing can drift out of step with anything else. It is not a second name for the token —
// the label is that, and the label is what the log carries. This is for the places a stable,
// fixed-width, delimiter-free identifier is needed.
func (t Token) ID() string {
	if len(t.Hash) < IDLen {
		return t.Hash
	}
	return t.Hash[:IDLen]
}

type file struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

// Store is the token file, and a copy of it held in memory.
type Store struct {
	path string

	// Log, when set, records a file that changed under this process. `docker exec notifio
	// notifio token add` is a second process, so this is the only trace the server has that
	// somebody minted or withdrew a credential.
	Log *slog.Logger

	mu      sync.Mutex
	tokens  []Token
	modTime time.Time
	size    int64
}

// Open reads the token file in dir. A file that is not there is a fresh volume, and means no
// tokens; a file that is there and unreadable is fatal.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(true); err != nil {
		return nil, err
	}
	return s, nil
}

// Path is the file this store reads and writes.
func (s *Store) Path() string { return s.path }

// reloadLocked re-reads the file when its mtime or size has moved. force reads regardless.
func (s *Store) reloadLocked(force bool) error {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.tokens, s.modTime, s.size = nil, time.Time{}, 0
		return nil
	}
	if err != nil {
		return err
	}
	if !force && info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return nil
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if f.Version != fileVersion && f.Version != fileVersionBearer {
		return fmt.Errorf("%s: version %d, want %d or %d", s.path, f.Version, fileVersionBearer, fileVersion)
	}
	for i, t := range f.Tokens {
		if t.Label == "" {
			return fmt.Errorf("%s: token %d has no label", s.path, i)
		}
		// A row without a channel is a file from before tokens were bound to one. It is a
		// load error rather than a token that can reach everything.
		if t.Channel == "" {
			return fmt.Errorf("%s: token %q has no channel", s.path, t.Label)
		}
	}
	if s.Log != nil && !force {
		labels := make([]string, 0, len(f.Tokens))
		for _, t := range f.Tokens {
			labels = append(labels, t.Label+"@"+t.Channel)
		}
		s.Log.Info("tokens changed", "count", len(f.Tokens), "tokens", labels)
	}
	s.tokens, s.modTime, s.size = f.Tokens, info.ModTime(), info.Size()
	return nil
}

// List returns the tokens, sorted by label.
func (s *Store) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(false); err != nil {
		return nil, err
	}
	out := slices.Clone(s.tokens)
	slices.SortFunc(out, func(a, b Token) int { return strings.Compare(a.Label, b.Label) })
	return out, nil
}

// Count is how many tokens exist.
func (s *Store) Count() (int, error) {
	list, err := s.List()
	return len(list), err
}

// Create mints a bearer token for one channel and returns the secret, which is stored only as
// a hash and cannot be recovered.
func (s *Store) Create(label, channel string) (string, error) {
	return s.create(label, channel, false)
}

// CreateNonced mints a token that authenticates by nonce.
//
// Its secret is kept in data.json in the clear, because verifying a SHA-256 means recomputing
// it. That is the trade: the secret never crosses the wire, and it sits on the disk instead.
func (s *Store) CreateNonced(label, channel string) (string, error) {
	return s.create(label, channel, true)
}

func (s *Store) create(label, channel string, nonced bool) (string, error) {
	if err := ValidLabel(label); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(true); err != nil {
		return "", err
	}
	if slices.ContainsFunc(s.tokens, func(t Token) bool { return t.Label == label }) {
		return "", fmt.Errorf("%q: %w", label, ErrConflict)
	}

	// Minted in a loop so an id collision cannot happen rather than being unlikely. Two
	// attempts is already beyond astronomical; ten is free.
	prefix := Prefix
	if nonced {
		prefix = SecretPrefix
	}
	var secret, hash string
	for attempt := 0; ; attempt++ {
		raw := make([]byte, secretBytes)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		secret = prefix + base64.RawURLEncoding.EncodeToString(raw)
		hash = hashOf(secret)
		id := Token{Hash: hash}.ID()
		if !slices.ContainsFunc(s.tokens, func(t Token) bool { return t.ID() == id }) {
			break
		}
		if attempt > 10 {
			return "", errors.New("could not mint a token with an unused id")
		}
	}

	row := Token{
		Label:     label,
		Channel:   channel,
		Hash:      hash,
		CreatedAt: time.Now().UTC().Unix(),
	}
	if nonced {
		row.Secret = secret
	}
	s.tokens = append(s.tokens, row)
	if err := s.writeLocked(); err != nil {
		return "", err
	}
	return secret, nil
}

// Remove deletes a token by label.
func (s *Store) Remove(label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(true); err != nil {
		return err
	}
	i := slices.IndexFunc(s.tokens, func(t Token) bool { return t.Label == label })
	if i < 0 {
		return fmt.Errorf("%q: %w", label, ErrNotFound)
	}
	s.tokens = slices.Delete(s.tokens, i, i+1)
	return s.writeLocked()
}

// ByID returns the token an id names.
func (s *Store) ByID(id string) (Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(false); err != nil {
		return Token{}, false, err
	}
	i := slices.IndexFunc(s.tokens, func(t Token) bool { return t.ID() == id })
	if i < 0 {
		return Token{}, false, nil
	}
	return s.tokens[i], true, nil
}

// Verify returns the token a secret belongs to.
//
// The error means the file could not be re-read, not that the token was wrong — folding those
// together would turn a damaged file into a silent outage.
func (s *Store) Verify(secret string) (Token, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(false); err != nil {
		return Token{}, false, err
	}
	if secret == "" {
		return Token{}, false, nil
	}

	want := hashOf(secret)
	// No early exit on a match: every stored hash is compared, so the time taken says nothing
	// about how close a guess was.
	var found Token
	ok := false
	for _, t := range s.tokens {
		// A nonced token's secret would hash to its stored hash, so without this it would work
		// as a bearer token and the whole point of it would be optional.
		if t.Nonced() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(want)) == 1 {
			found, ok = t, true
		}
	}
	return found, ok, nil
}

// VerifyNonced checks what a nonced token puts on the wire:
//
//	ntc_<unix seconds>.<token id>.<sha256 hex of "<unix seconds>.<token id>.<secret>">
//
// The secret goes last in the hashed base, which is what keeps SHA-256's length-extension
// property from mattering here: a forged digest would be for a shape the server never builds.
//
// A false result means the value was not good. The error means the file could not be re-read.
func (s *Store) VerifyNonced(wire string, now time.Time) (Token, bool, error) {
	rest, ok := strings.CutPrefix(wire, NoncedPrefix)
	if !ok {
		return Token{}, false, nil
	}
	parts := strings.Split(rest, Sep)
	if len(parts) != 3 {
		return Token{}, false, nil
	}
	nonce, id, mac := parts[0], parts[1], parts[2]

	// Each field is checked for shape before anything is looked up, so the base cannot be made
	// ambiguous by a value that merely looks like one.
	if !isDigits(nonce) || len(nonce) > 20 {
		return Token{}, false, nil
	}
	if len(id) != IDLen || !isHex(id) {
		return Token{}, false, nil
	}
	if len(mac) != sha256.Size*2 || !isHex(mac) {
		return Token{}, false, nil
	}
	ts, err := strconv.ParseInt(nonce, 10, 64)
	if err != nil {
		return Token{}, false, nil
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Window || d < -Window {
		return Token{}, false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(false); err != nil {
		return Token{}, false, err
	}
	var found Token
	matched := false
	for _, t := range s.tokens {
		if !t.Nonced() || t.ID() != id {
			continue
		}
		want := hashOf(nonce + Sep + id + Sep + t.Secret)
		if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) == 1 {
			found, matched = t, true
		}
	}
	return found, matched, nil
}

// Sign builds what a caller puts on the wire. It exists so the server can prove its own
// documentation, and so `channel test` and the tests do not hand-roll the format.
func Sign(id, secret string, now time.Time) string {
	nonce := strconv.FormatInt(now.UTC().Unix(), 10)
	return NoncedPrefix + nonce + Sep + id + Sep + hashOf(nonce+Sep+id+Sep+secret)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// writeLocked replaces the file atomically, so a reader never sees a half-written one.
func (s *Store) writeLocked() error {
	// Only claim version 2 once something in the file needs it.
	f := file{Version: fileVersionBearer, Tokens: s.tokens}
	if slices.ContainsFunc(s.tokens, Token.Nonced) {
		f.Version = fileVersion
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}

	if info, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = info.ModTime(), info.Size()
	}
	return nil
}

// ValidLabel reports whether a label can name a token.
func ValidLabel(label string) error {
	switch {
	case label == "":
		return fmt.Errorf("a label is required: %w", ErrInvalid)
	case len(label) > labelMax:
		return fmt.Errorf("a label is at most %d characters: %w", labelMax, ErrInvalid)
	case strings.TrimSpace(label) != label:
		return fmt.Errorf("a label has no leading or trailing space: %w", ErrInvalid)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("a label has no control characters: %w", ErrInvalid)
		}
	}
	return nil
}

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
