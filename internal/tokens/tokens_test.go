package tokens

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestMissingFileIsZeroTokens(t *testing.T) {
	s, _ := open(t)
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("a fresh volume holds %d tokens", len(list))
	}
}

func TestRoundTrip(t *testing.T) {
	s, _ := open(t)
	secret, err := s.Create("grafana", "alerts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, Prefix) {
		t.Errorf("secret %q has no %q prefix", secret, Prefix)
	}

	tok, ok, err := s.Verify(secret)
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
	if tok.Label != "grafana" || tok.Channel != "alerts" {
		t.Errorf("Verify returned %+v", tok)
	}
	if _, ok, _ := s.Verify(secret + "x"); ok {
		t.Error("a wrong secret verified")
	}
	if _, ok, _ := s.Verify(""); ok {
		t.Error("an empty secret verified")
	}
}

// The secret is not anywhere, including in the file it is checked against.
func TestTheSecretIsNotStored(t *testing.T) {
	s, dir := open(t)
	secret, err := s.Create("a", "alerts")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("the secret is in data.json")
	}
}

// Labels are unique across the file, not per channel, so remove takes one argument.
func TestDuplicateLabelIsRefusedAcrossChannels(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Create("grafana", "alerts"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create("grafana", "notices")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second Create = %v, want ErrConflict", err)
	}
}

func TestRemove(t *testing.T) {
	s, _ := open(t)
	secret, _ := s.Create("a", "alerts")
	if err := s.Remove("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing an unknown label = %v, want ErrNotFound", err)
	}
	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Verify(secret); ok {
		t.Error("a removed token still verifies")
	}
}

// A row from before tokens were bound to a channel is a load error, not a token that can reach
// everything.
func TestARowWithNoChannelIsALoadError(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":1,"tokens":[{"label":"old","hash":"deadbeef","created_at":1}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "no channel") {
		t.Fatalf("Open = %v, want a complaint about the missing channel", err)
	}
}

// docker exec is a second process: the running server has to see what it wrote, with no
// restart.
func TestASecondProcessIsSeen(t *testing.T) {
	dir := t.TempDir()
	server, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	secret, err := writer.Create("late", "alerts")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := server.Verify(secret); err != nil || !ok {
		t.Fatalf("the server did not see a token minted beside it: %v, %v", ok, err)
	}

	if err := writer.Remove("late"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := server.Verify(secret); ok {
		t.Error("the server still accepts a withdrawn token")
	}
}

func TestFileIsPrivate(t *testing.T) {
	s, dir := open(t)
	if _, err := s.Create("a", "alerts"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("data.json is %v, want 0600", perm)
	}
}

func TestValidLabel(t *testing.T) {
	for _, bad := range []string{"", " leading", "trailing ", strings.Repeat("x", labelMax+1), "line\nbreak"} {
		if err := ValidLabel(bad); err == nil {
			t.Errorf("accepted label %q", bad)
		}
	}
	if err := ValidLabel("grafana-tg_1"); err != nil {
		t.Errorf("rejected a fine label: %v", err)
	}
}

// The id names a token without naming its secret or its label, and every file that already
// exists has one because it is derived from the hash rather than stored.
func TestID(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Create("grafana", "alerts"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	tok := list[0]

	if len(tok.ID()) != IDLen {
		t.Errorf("id = %q, want %d characters", tok.ID(), IDLen)
	}
	if tok.ID() != tok.Hash[:IDLen] {
		t.Errorf("id %q is not the head of the hash %q", tok.ID(), tok.Hash)
	}
	// Fixed width and delimiter-free, which is what makes it usable inside a wire format.
	for _, r := range tok.ID() {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("id %q carries %q, which is not hex", tok.ID(), string(r))
		}
	}

	got, ok, err := s.ByID(tok.ID())
	if err != nil || !ok {
		t.Fatalf("ByID = %v, %v", ok, err)
	}
	if got.Label != "grafana" {
		t.Errorf("ByID returned %+v", got)
	}
	if _, ok, _ := s.ByID("deadbeef"); ok {
		t.Error("an unknown id resolved")
	}
	if _, ok, _ := s.ByID(""); ok {
		t.Error("an empty id resolved")
	}
}

// A row from a file written before ids existed still has one.
func TestIDNeedsNoMigration(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":1,"tokens":[{"label":"old","channel":"alerts",` +
		`"hash":"2c6da8e2234b6e28e7657e65941fcd7888a7baa6db6575c42abbcefde9899c0c","created_at":1}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].ID(); got != "2c6da8e2" {
		t.Errorf("id = %q, want 2c6da8e2", got)
	}
}

// Every token in a file has a distinct id, because Create re-mints on a clash.
func TestIDsAreDistinct(t *testing.T) {
	s, _ := open(t)
	seen := map[string]bool{}
	for i := range 50 {
		if _, err := s.Create(fmt.Sprintf("t%d", i), "alerts"); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if seen[tok.ID()] {
			t.Fatalf("id %q appears twice", tok.ID())
		}
		seen[tok.ID()] = true
	}
	if len(seen) != 50 {
		t.Errorf("%d distinct ids for 50 tokens", len(seen))
	}
}
