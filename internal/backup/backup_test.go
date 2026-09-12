package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type received struct {
	Name    string
	Fields  map[string]string
	Auth    string
	Query   string
	Archive []byte
}

type agent struct {
	mu     sync.Mutex
	got    []received
	status int
	body   string
}

func newAgent(t *testing.T) (*agent, string) {
	a := &agent{status: 200, body: `{"status":"ok","destination":"gdrive:notifio/prod/notifio-20260912_040506.tgz"}`}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, srv.URL + "/backup"
}

func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(1 << 20)
	rec := received{
		Auth:   r.Header.Get("Authorization"),
		Query:  r.URL.RawQuery,
		Fields: map[string]string{},
	}
	// FormValue sees query parameters as well as body fields, which is what lets one URL
	// carry the agent's provider and subdirectory.
	for _, k := range []string{"name", "provider", "subdirectory"} {
		rec.Fields[k] = r.FormValue(k)
	}
	if f, h, err := r.FormFile("backup"); err == nil {
		rec.Name = h.Filename
		rec.Archive, _ = io.ReadAll(f)
		f.Close()
	}

	a.mu.Lock()
	a.got = append(a.got, rec)
	status, body := a.status, a.body
	a.mu.Unlock()

	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (a *agent) seen() []received {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.got
}

func entries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Mode != 0o600 {
			t.Errorf("entry %q is mode %o, want 600", h.Name, h.Mode)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
	return out
}

func newPusher(t *testing.T, url string) (*Pusher, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	data := filepath.Join(dir, "data.json")
	os.WriteFile(cfg, []byte(`{"version":1}`), 0o600)
	os.WriteFile(data, []byte(`{"version":1,"tokens":[]}`), 0o600)
	return &Pusher{
		URL: url, ConfigPath: cfg, DataPath: data,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, cfg, data
}

func TestTheArchiveHoldsExactlyTheTwoFiles(t *testing.T) {
	a, url := newAgent(t)
	p, _, _ := newPusher(t, url)

	if _, err := p.Once(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	got := a.seen()
	if len(got) != 1 {
		t.Fatalf("the agent saw %d pushes", len(got))
	}
	files := entries(t, got[0].Archive)
	if len(files) != 2 || files["config.json"] == "" || files["data.json"] == "" {
		t.Errorf("archive holds %v", keys(files))
	}
}

// A fresh volume has no data.json yet, and that is not a failure.
func TestAMissingFileIsSkipped(t *testing.T) {
	a, url := newAgent(t)
	p, _, data := newPusher(t, url)
	os.Remove(data)

	if _, err := p.Once(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	files := entries(t, a.seen()[0].Archive)
	if len(files) != 1 || files["config.json"] == "" {
		t.Errorf("archive holds %v", keys(files))
	}
}

// backio-agent's contract: an archive, a name, and nothing else.
//
// The agent holds the token and decides where archives land, so a compromised notifio cannot
// reach a single existing backup. Sending a credential or a destination would be notifio
// claiming authority it deliberately does not have.
func TestTheRequestCarriesNoCredentialAndNoDestination(t *testing.T) {
	a, url := newAgent(t)
	p, _, _ := newPusher(t, url)
	if _, err := p.Once(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	got := a.seen()[0]
	if got.Auth != "" {
		t.Errorf("Authorization = %q; the sidecar takes none", got.Auth)
	}
	if got.Query != "" {
		t.Errorf("query = %q; everything goes in the body", got.Query)
	}
	for _, f := range []string{"provider", "subdirectory"} {
		if got.Fields[f] != "" {
			t.Errorf("%s = %q; that is the sidecar's decision, not ours", f, got.Fields[f])
		}
	}
	if got.Fields["name"] != Name || got.Name != Name {
		t.Errorf("name field = %q, file name = %q, want %q", got.Fields["name"], got.Name, Name)
	}
}

// A restart that rewrites nothing must not fill a remote with identical files.
func TestUnchangedFilesSendNothing(t *testing.T) {
	a, url := newAgent(t)
	p, cfg, _ := newPusher(t, url)

	if _, err := p.Once(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Once(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := len(a.seen()); n != 1 {
		t.Fatalf("%d pushes for an unchanged pair, want 1", n)
	}

	// Rewritten with identical content: the mtime moved and the digest did not.
	body, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, body, 0o600)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(cfg, future, future)
	if _, err := p.Once(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := len(a.seen()); n != 1 {
		t.Errorf("an identical rewrite produced a push (%d total)", n)
	}

	// One byte different, and it goes.
	os.WriteFile(cfg, append(body, ' '), 0o600)
	if _, err := p.Once(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := len(a.seen()); n != 2 {
		t.Errorf("a changed file produced %d pushes, want 2", n)
	}
}

func TestForceSendsRegardless(t *testing.T) {
	a, url := newAgent(t)
	p, _, _ := newPusher(t, url)
	for range 3 {
		if _, err := p.Once(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.seen()); n != 3 {
		t.Errorf("%d pushes, want 3", n)
	}
}

// "The agent answered 500" is a fact nobody can act on.
func TestTheAgentsOwnWordsReachTheError(t *testing.T) {
	a, url := newAgent(t)
	a.status, a.body = 403, "token lacks create permission for gdrive"
	p, _, _ := newPusher(t, url)

	_, err := p.Once(context.Background(), true)
	if err == nil {
		t.Fatal("a 403 was treated as success")
	}
	if !strings.Contains(err.Error(), "token lacks create permission") {
		t.Errorf("err = %q", err)
	}
}

// A failed push must not leave the digest advanced, or the next change would be skipped.
func TestAFailedPushIsRetriedNextPass(t *testing.T) {
	a, url := newAgent(t)
	a.status = 500
	p, _, _ := newPusher(t, url)

	if _, err := p.Once(context.Background(), false); err == nil {
		t.Fatal("expected a failure")
	}
	a.mu.Lock()
	a.status, a.body = 200, `{"status":"ok"}`
	a.mu.Unlock()

	if _, err := p.Once(context.Background(), false); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if n := len(a.seen()); n != 2 {
		t.Errorf("%d attempts, want the first to be retried", n)
	}
}

// The agent reads the name for its extension only, and assigns its own timestamped filename.
func TestNameCarriesTheExtension(t *testing.T) {
	if !strings.HasSuffix(Name, ".tgz") {
		t.Errorf("Name = %q; the agent names the file from this extension", Name)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
