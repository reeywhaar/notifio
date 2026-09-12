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
	"os"
	"path/filepath"
	"slices"
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

	// Prefix marks a notifio token in a log somebody is grepping.
	Prefix = "nt_"

	// secretBytes is 256 bits, which is why the hash below can be a fast one.
	secretBytes = 32

	// fileVersion is written into the file so a format change is a migration. The change it
	// is kept for is a token carrying several channels.
	fileVersion = 1

	labelMax = 64
)

// Token is one entry: what it is called, where it may send, and enough to recognise it.
type Token struct {
	Label     string `json:"label"`
	Channel   string `json:"channel"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

// Created is CreatedAt as a time, in UTC like everything else here.
func (t Token) Created() time.Time { return time.Unix(t.CreatedAt, 0).UTC() }

type file struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`
}

// Store is the token file, and a copy of it held in memory.
type Store struct {
	path string

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
	if f.Version != fileVersion {
		return fmt.Errorf("%s: version %d, want %d", s.path, f.Version, fileVersion)
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

// Create mints a token for one channel and returns the secret, which is not stored and cannot
// be recovered.
func (s *Store) Create(label, channel string) (string, error) {
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

	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := Prefix + base64.RawURLEncoding.EncodeToString(raw)

	s.tokens = append(s.tokens, Token{
		Label:     label,
		Channel:   channel,
		Hash:      hashOf(secret),
		CreatedAt: time.Now().UTC().Unix(),
	})
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
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(want)) == 1 {
			found, ok = t, true
		}
	}
	return found, ok, nil
}

// writeLocked replaces the file atomically, so a reader never sees a half-written one.
func (s *Store) writeLocked() error {
	f := file{Version: fileVersion, Tokens: s.tokens}
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
