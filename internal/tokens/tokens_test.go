package tokens

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestNoncedRoundTrip(t *testing.T) {
	s, _ := open(t)
	secret, err := s.Create("ci", "tg")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	wire := Sign(secret, now)
	if !strings.HasPrefix(wire, NoncedPrefix) {
		t.Errorf("wire value %q has no %q prefix", wire, NoncedPrefix)
	}
	if n := strings.Count(wire, Sep); n != 2 {
		t.Errorf("wire value %q has %d separators, want 2", wire, n)
	}

	tok, ok, err := s.VerifyNonced(wire, now)
	if err != nil || !ok {
		t.Fatalf("VerifyNonced = %v, %v", ok, err)
	}
	if tok.Label != "ci" || tok.Channel != "tg" {
		t.Errorf("returned %+v", tok)
	}
}

// The whole point: what crosses the wire is not the credential.
func TestTheWireValueIsNotTheSecret(t *testing.T) {
	s, _ := open(t)
	secret, _ := s.Create("ci", "tg")
	wire := Sign(secret, time.Now())

	if strings.Contains(wire, secret) {
		t.Fatal("the secret is in the wire value")
	}
	// And a captured one is useless once the window passes.
	if _, ok, _ := s.VerifyNonced(wire, time.Now().Add(Window+time.Minute)); ok {
		t.Error("a stale nonce was accepted")
	}
	if _, ok, _ := s.VerifyNonced(wire, time.Now().Add(-Window-time.Minute)); ok {
		t.Error("a nonce from the future was accepted")
	}
	// Inside the window it still works, in both directions, for clock skew.
	for _, skew := range []time.Duration{-Window + time.Second, 0, Window - time.Second} {
		if _, ok, _ := s.VerifyNonced(wire, time.Now().Add(skew)); !ok {
			t.Errorf("a nonce %v out was refused", skew)
		}
	}
}

func TestNoncedRejections(t *testing.T) {
	s, _ := open(t)
	secret, _ := s.Create("ci", "tg")
	list, _ := s.List()
	id := list[0].ID()
	now := time.Now()
	good := Sign(secret, now)

	cases := map[string]string{
		"no prefix":        strings.TrimPrefix(good, NoncedPrefix),
		"bearer prefix":    Prefix + strings.TrimPrefix(good, NoncedPrefix),
		"empty":            "",
		"too few fields":   NoncedPrefix + "123" + Sep + id,
		"too many fields":  good + Sep + "extra",
		"nonce not digits": good[:len(NoncedPrefix)] + "12a4567890" + good[len(NoncedPrefix)+10:],
		"unknown id":       NoncedPrefix + "1789310655" + Sep + "deadbeef" + Sep + strings.Repeat("a", 64),
		"wrong mac":        good[:len(good)-1] + map[bool]string{true: "0", false: "1"}[good[len(good)-1] == '1'],
		"short mac":        NoncedPrefix + "1789310655" + Sep + id + Sep + "abc",
		"uppercase mac":    strings.ToUpper(good),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok, _ := s.VerifyNonced(wire, now); ok {
				t.Errorf("accepted %q", wire)
			}
		})
	}
}

// One token, two ways to present it. That is the whole feature.
func TestOneTokenBothWays(t *testing.T) {
	s, _ := open(t)
	secret, err := s.Create("ci", "tg")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := s.Verify(secret); !ok {
		t.Error("the secret does not work as a bearer token")
	}
	if _, ok, _ := s.VerifyNonced(Sign(secret, time.Now()), time.Now()); !ok {
		t.Error("the same secret does not work hashed with a nonce")
	}
}

// Nothing recoverable is stored: the file holds a hash, as it always did.
func TestTheFileStillHoldsOnlyAHash(t *testing.T) {
	s, dir := open(t)
	secret, err := s.Create("ci", "tg")
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
	var f struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Version != 1 {
		t.Errorf("version = %d; nothing about the format changed", f.Version)
	}
}
