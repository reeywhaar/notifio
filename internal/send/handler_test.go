package send

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"notifio/internal/config"
	"notifio/internal/tokens"
)

type harness struct {
	h      *Handler
	srv    *httptest.Server
	logs   *bytes.Buffer
	dir    string
	cfgDir string
	tokens *tokens.Store
	tg     *httptest.Server
}

// telegramOK stands in for the Bot API so a handler test needs no network.
func telegramOK(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"message_id":4821}}`)
	}))
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tg := telegramOK(t)
	t.Cleanup(tg.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := `{"version":1,"channels":{
	  "alerts":{"type":"telegram","token":"1:x","pinned":{"to":"-1001234567890"},"api_base":"` + tg.URL + `"},
	  "notices":{"type":"email","host":"127.0.0.1","port":1,"encryption":"none","pinned":{"from":"n@example.com"}},
	  "tiny":{"type":"telegram","token":"1:x","pinned":{"to":"-5"},"api_base":"` + tg.URL + `","max_body":"512"}
	}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := tokens.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	h := &Handler{
		Config: cfg, Tokens: st, Client: tg.Client(),
		Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return &harness{h: h, srv: srv, logs: logs, dir: dir, cfgDir: cfgPath, tokens: st, tg: tg}
}

func (h *harness) mint(t *testing.T, label, channel string) string {
	t.Helper()
	secret, err := h.tokens.Create(label, channel)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func (h *harness) post(t *testing.T, token string, values url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/send", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func body(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	defer res.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatalf("response was not JSON: %v", err)
	}
	return m
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	res, err := h.srv.Client().Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	m := body(t, res)
	if res.StatusCode != 200 || m["ok"] != true || m["channels"].(float64) != 3 {
		t.Errorf("healthz = %d %v", res.StatusCode, m)
	}
}

func TestRouting(t *testing.T) {
	h := newHarness(t)
	res, _ := h.srv.Client().Get(h.srv.URL + "/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d", res.StatusCode)
	}
	res.Body.Close()

	res, _ = h.srv.Client().Get(h.srv.URL + "/api/send")
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "POST" {
		t.Errorf("GET /api/send = %d, Allow %q", res.StatusCode, res.Header.Get("Allow"))
	}
	res.Body.Close()
}

func TestAuth(t *testing.T) {
	h := newHarness(t)
	h.mint(t, "grafana", "alerts")

	for _, tok := range []string{"", "nt_wrong"} {
		res := h.post(t, tok, url.Values{"body": {"x"}})
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", tok, res.StatusCode)
		}
		if code := body(t, res)["code"]; code != "auth" {
			t.Errorf("code = %v", code)
		}
	}
}

// The unauthenticated-upload hole: a stranger must not be able to make notifio read a body.
func TestAuthIsCheckedBeforeTheBodyIsRead(t *testing.T) {
	h := newHarness(t)

	counted := &countingReader{r: strings.NewReader(strings.Repeat("x", 1<<20))}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/send", counted)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = 1 << 20
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	// The client may have pushed some bytes onto the wire before the server answered; what
	// matters is that the server did not ask for the whole megabyte.
	if n := counted.n.Load(); n == 1<<20 {
		t.Errorf("the whole body was read from an unauthenticated request (%d bytes)", n)
	}
}

func TestSendThroughTelegram(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "grafana", "alerts")

	res := h.post(t, tok, url.Values{"body": {"Disk at 91%"}})
	m := body(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d: %v", res.StatusCode, m)
	}
	if m["ok"] != true || m["channel"] != "alerts" || m["type"] != "telegram" || m["id"] != "4821" {
		t.Errorf("response = %v", m)
	}
}

// The validator is picked by the token, not by the body.
func TestTheTokenPicksTheValidator(t *testing.T) {
	h := newHarness(t)
	tg := h.mint(t, "tg", "alerts")
	em := h.mint(t, "em", "notices")

	res := h.post(t, tg, url.Values{"body": {"x"}, "from": {"a@x.com"}})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("from on a telegram token = %d, want 400", res.StatusCode)
	}
	res.Body.Close()

	res = h.post(t, em, url.Values{"body": {"x"}, "to": {"a@x.com"}, "body_type": {"sgml"}})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown body_type = %d, want 400", res.StatusCode)
	}
	res.Body.Close()
}

func TestChannelInTheBodyIsRefused(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "grafana", "alerts")
	res := h.post(t, tok, url.Values{"body": {"x"}, "channel": {"alerts"}})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 even for the right channel", res.StatusCode)
	}
}

// Not 401: hunting for a revoked credential that is working fine is an hour lost.
func TestAnOrphanedTokenIs503(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "grafana", "alerts")

	rest := `{"version":1,"channels":{"notices":{"type":"email","host":"127.0.0.1","port":1,"encryption":"none","pinned":{"from":"n@example.com"}}}}`
	if err := os.WriteFile(h.cfgDir, []byte(rest), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, h.cfgDir)

	res := h.post(t, tok, url.Values{"body": {"x"}})
	m := body(t, res)
	if res.StatusCode != http.StatusServiceUnavailable || m["code"] != "config" {
		t.Fatalf("status = %d, body = %v; want 503 config", res.StatusCode, m)
	}
}

// The cap is the channel's own, and it is installed before the body is parsed.
func TestPerChannelMaxBody(t *testing.T) {
	h := newHarness(t)
	big := h.mint(t, "big", "alerts")
	small := h.mint(t, "small", "tiny")
	payload := url.Values{"body": {strings.Repeat("x", 2000)}}

	if res := h.post(t, big, payload); res.StatusCode != 200 {
		t.Errorf("the 25MiB channel = %d, want 200", res.StatusCode)
		res.Body.Close()
	}
	res := h.post(t, small, payload)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("the 512B channel = %d, want 413", res.StatusCode)
	}
	res.Body.Close()
}

func TestUnsupportedMediaType(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "grafana", "alerts")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/send", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", res.StatusCode)
	}
	res.Body.Close()
}

// A provider that will not answer is 502, not 400: the caller did nothing wrong.
func TestAProviderFailureIs502(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "em", "notices")
	res := h.post(t, tok, url.Values{"to": {"a@x.com"}, "subject": {"s"}, "body": {"x"}})
	m := body(t, res)
	if res.StatusCode != http.StatusBadGateway || m["code"] != "provider" {
		t.Fatalf("status = %d, body = %v; want 502 provider", res.StatusCode, m)
	}
}

// The logging guarantee, asserted rather than intended.
func TestTheLogCarriesNoSecrets(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "grafana", "alerts")
	h.post(t, tok, url.Values{"body": {"the-secret-body-text"}}).Body.Close()
	h.post(t, "nt_wrongtoken", url.Values{"body": {"x"}}).Body.Close()

	em := h.mint(t, "em", "notices")
	h.post(t, em, url.Values{"to": {"person@example.com"}, "subject": {"the-secret-subject"}, "body": {"b"}}).Body.Close()

	logged := h.logs.String()
	for _, secret := range []string{
		"the-secret-body-text", "the-secret-subject", tok, em, "nt_wrongtoken", "person@example.com",
	} {
		if strings.Contains(logged, secret) {
			t.Errorf("the log carries %q", secret)
		}
	}
	if !strings.Contains(logged, `"token":"grafana"`) {
		t.Error("the log does not carry the token's label, which is what labels are for")
	}
	if !strings.Contains(logged, "@example.com") {
		t.Error("the log does not carry the recipient's domain")
	}
}

// A token minted beside the server is accepted with no restart; a withdrawn one stops working.
func TestTokenReload(t *testing.T) {
	h := newHarness(t)
	beside, err := tokens.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := beside.Create("late", "alerts")
	if err != nil {
		t.Fatal(err)
	}

	if res := h.post(t, secret, url.Values{"body": {"x"}}); res.StatusCode != 200 {
		t.Errorf("a token minted beside the server = %d, want 200", res.StatusCode)
		res.Body.Close()
	}
	if err := beside.Remove("late"); err != nil {
		t.Fatal(err)
	}
	res := h.post(t, secret, url.Values{"body": {"x"}})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a withdrawn token = %d, want 401", res.StatusCode)
	}
	res.Body.Close()
}

// Two tokens, two channels, and neither reaches the other's.
func TestTokensReachOnlyTheirOwnChannel(t *testing.T) {
	h := newHarness(t)
	tg := h.mint(t, "tg", "alerts")
	em := h.mint(t, "em", "notices")

	// An email-shaped request on the telegram token is refused rather than routed.
	res := h.post(t, tg, url.Values{"to": {"a@x.com"}, "subject": {"s"}, "body": {"x"}})
	if res.StatusCode == 200 {
		t.Error("a telegram token accepted an email-shaped request")
	}
	res.Body.Close()

	// And the email token never reaches the telegram stub, which is the only thing that
	// answers 200 in this harness.
	res = h.post(t, em, url.Values{"to": {"a@x.com"}, "subject": {"s"}, "body": {"x"}})
	if res.StatusCode == 200 {
		t.Error("an email token reached something that answered 200")
	}
	res.Body.Close()
}

func touch(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	future := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

// countingReader records how much of a body the server actually asked for.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Breaking config.json must not be silent, and must not look like an empty instance.
//
// The first version of this reported channels:0 from healthz, logged nothing at all, and went
// on sending — because healthz consumed the mtime change and threw the error away, so the next
// caller saw a clean result over a file that was still broken.
func TestABrokenConfigIsLoudAndHarmless(t *testing.T) {
	h := newHarness(t)
	tok := h.mint(t, "grafana", "alerts")

	if res := h.post(t, tok, url.Values{"body": {"x"}}); res.StatusCode != 200 {
		t.Fatalf("sending was broken before the test began: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	if err := os.WriteFile(h.cfgDir, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, h.cfgDir)

	// healthz first: this is the call that used to swallow the error.
	res, err := h.srv.Client().Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	m := body(t, res)
	if got := m["channels"].(float64); got != 3 {
		t.Errorf("healthz reports %v channels; it is still serving 3", got)
	}
	if m["config"] != "stale" {
		t.Errorf("healthz does not say the config is stale: %v", m)
	}
	if res.StatusCode != 200 {
		t.Errorf("healthz = %d; a 503 would restart into a container that cannot start", res.StatusCode)
	}

	// Sending still works on the last good config.
	if res := h.post(t, tok, url.Values{"body": {"x"}}); res.StatusCode != 200 {
		t.Errorf("a broken config stopped a channel that was already loaded: %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	if n := strings.Count(h.logs.String(), "config will not load"); n != 1 {
		t.Errorf("the failure was logged %d times, want exactly 1", n)
	}

	// Repeated calls do not repeat the line.
	for range 3 {
		h.srv.Client().Get(h.srv.URL + "/healthz")
	}
	if n := strings.Count(h.logs.String(), "config will not load"); n != 1 {
		t.Errorf("the log line repeats on every healthcheck (%d times)", n)
	}

	// And fixing it is reported too.
	good := `{"version":1,"channels":{"alerts":{"type":"telegram","token":"1:x","pinned":{"to":"-1"},"api_base":"` + h.tg.URL + `"}}}`
	if err := os.WriteFile(h.cfgDir, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, h.cfgDir)
	h.srv.Client().Get(h.srv.URL + "/healthz")
	if !strings.Contains(h.logs.String(), "config loaded again") {
		t.Error("recovery was not reported")
	}
}
