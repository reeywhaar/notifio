package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"notifio/internal/channel"
	"notifio/internal/config"
)

// call is one request the stub received.
type call struct {
	Method string
	Form   url.Values
	Files  map[string]string
}

type stub struct {
	mu     sync.Mutex
	calls  []call
	answer func(method string) (int, string)
}

func newStub(t *testing.T) (*stub, *config.Channel, *http.Client) {
	s := &stub{answer: func(string) (int, string) {
		return 200, `{"ok":true,"result":{"message_id":4821}}`
	}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	ch := &config.Channel{
		Name: "alerts", Type: config.TypeTelegram, Token: "123:abc", APIBase: srv.URL,
		MaxBody: config.DefaultMaxBody, SendTimeout: config.Duration(config.DefaultSendTimeout),
	}
	return s, ch, srv.Client()
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := call{Method: r.URL.Path, Form: url.Values{}, Files: map[string]string{}}

	ct := r.Header.Get("Content-Type")
	if mt, params, _ := mime.ParseMediaType(ct); mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(p)
			if p.FileName() != "" {
				c.Files[p.FormName()] = p.FileName()
			} else {
				c.Form.Set(p.FormName(), string(body))
			}
		}
	} else {
		r.ParseForm()
		c.Form = r.PostForm
	}

	s.mu.Lock()
	s.calls = append(s.calls, c)
	answer := s.answer
	s.mu.Unlock()

	code, body := answer(c.Method)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (s *stub) seen() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func att(name, body string) channel.Attachment {
	return channel.Attachment{
		Filename: name, ContentType: "text/plain", Size: int64(len(body)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil },
	}
}

func send(t *testing.T, s *stub, ch *config.Channel, client *http.Client, m *channel.Message) (*channel.Result, error) {
	t.Helper()
	return New(ch, client).Send(context.Background(), m)
}

// The method table: 0, 1 and many attachments pick three different calls.
func TestMethodSelection(t *testing.T) {
	cases := []struct {
		atts int
		want string
	}{{0, "sendMessage"}, {1, "sendDocument"}, {2, "sendMediaGroup"}, {10, "sendMediaGroup"}}
	for _, c := range cases {
		s, ch, client := newStub(t)
		m := &channel.Message{To: []string{"-100"}, Body: "hi"}
		for i := range c.atts {
			m.Atts = append(m.Atts, att(string(rune('a'+i))+".txt", "data"))
		}
		if _, err := send(t, s, ch, client, m); err != nil {
			t.Fatal(err)
		}
		calls := s.seen()
		if len(calls) != 1 {
			t.Fatalf("%d attachments made %d calls", c.atts, len(calls))
		}
		if !strings.HasSuffix(calls[0].Method, "/"+c.want) {
			t.Errorf("%d attachments called %q, want %q", c.atts, calls[0].Method, c.want)
		}
	}
}

func TestTheURLCarriesTheBotToken(t *testing.T) {
	s, ch, client := newStub(t)
	if _, err := send(t, s, ch, client, &channel.Message{To: []string{"-100"}, Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got := s.seen()[0].Method; got != "/bot123:abc/sendMessage" {
		t.Errorf("path = %q", got)
	}
}

func TestParseModeMapping(t *testing.T) {
	cases := map[string]string{"plain": "", "md": "MarkdownV2", "html": "HTML"}
	for bodyType, want := range cases {
		s, ch, client := newStub(t)
		if _, err := send(t, s, ch, client, &channel.Message{To: []string{"-100"}, Body: "hi", BodyType: bodyType}); err != nil {
			t.Fatal(err)
		}
		if got := s.seen()[0].Form.Get("parse_mode"); got != want {
			t.Errorf("body_type %q sent parse_mode %q, want %q", bodyType, got, want)
		}
	}
}

// A body too long for a caption goes as its own message first, so the text arrives above its
// attachments rather than being truncated.
func TestALongBodyIsSentAsItsOwnMessageFirst(t *testing.T) {
	s, ch, client := newStub(t)
	long := strings.Repeat("x", maxCaption+1)
	if _, err := send(t, s, ch, client, &channel.Message{
		To: []string{"-100"}, Body: long, Atts: []channel.Attachment{att("a.txt", "d")}}); err != nil {
		t.Fatal(err)
	}

	calls := s.seen()
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want 2", len(calls))
	}
	if !strings.HasSuffix(calls[0].Method, "/sendMessage") {
		t.Errorf("first call was %q, want sendMessage", calls[0].Method)
	}
	if calls[0].Form.Get("text") != long {
		t.Error("the text was not sent whole")
	}
	if !strings.HasSuffix(calls[1].Method, "/sendDocument") {
		t.Errorf("second call was %q", calls[1].Method)
	}
	if calls[1].Form.Get("caption") != "" {
		t.Error("the document also carried a caption")
	}
}

func TestAShortBodyRidesAsACaption(t *testing.T) {
	s, ch, client := newStub(t)
	if _, err := send(t, s, ch, client, &channel.Message{
		To: []string{"-100"}, Body: "short", Atts: []channel.Attachment{att("a.txt", "d")}}); err != nil {
		t.Fatal(err)
	}
	calls := s.seen()
	if len(calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(calls))
	}
	if calls[0].Form.Get("caption") != "short" {
		t.Errorf("caption = %q", calls[0].Form.Get("caption"))
	}
}

// The API's one non-obvious corner: media entries name their parts with attach://.
func TestMediaGroupReferencesItsParts(t *testing.T) {
	s, ch, client := newStub(t)
	m := &channel.Message{To: []string{"-100"}, Body: "cap", BodyType: "html",
		Atts: []channel.Attachment{att("a.txt", "one"), att("b.txt", "two"), att("c.txt", "three")}}
	if _, err := send(t, s, ch, client, m); err != nil {
		t.Fatal(err)
	}

	c := s.seen()[0]
	var media []map[string]string
	if err := json.Unmarshal([]byte(c.Form.Get("media")), &media); err != nil {
		t.Fatalf("media is not JSON: %v", err)
	}
	if len(media) != 3 {
		t.Fatalf("media has %d entries", len(media))
	}
	for i, entry := range media {
		if entry["type"] != "document" {
			t.Errorf("entry %d is %q; everything is a document", i, entry["type"])
		}
		field := strings.TrimPrefix(entry["media"], "attach://")
		if field == entry["media"] {
			t.Fatalf("entry %d does not use attach://: %q", i, entry["media"])
		}
		if _, ok := c.Files[field]; !ok {
			t.Errorf("entry %d names part %q, which was not sent", i, field)
		}
	}
	if media[0]["caption"] != "cap" || media[0]["parse_mode"] != "HTML" {
		t.Errorf("the caption is not on the first entry: %v", media[0])
	}
	if _, ok := media[1]["caption"]; ok {
		t.Error("a second entry also carries a caption")
	}
}

// The description is the only thing that says what to fix.
func TestTheDescriptionIsRelayedVerbatim(t *testing.T) {
	s, ch, client := newStub(t)
	s.answer = func(string) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	}
	_, err := send(t, s, ch, client, &channel.Message{To: []string{"-100"}, Body: "hi"})
	if !errors.Is(err, channel.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "Bad Request: chat not found") {
		t.Errorf("err = %q", err)
	}
}

// Not slept on: holding a caller's request open through an incident runs out of sockets.
func TestRetryAfterIsReportedRatherThanWaitedOut(t *testing.T) {
	s, ch, client := newStub(t)
	s.answer = func(string) (int, string) {
		return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":30}}`
	}
	_, err := send(t, s, ch, client, &channel.Message{To: []string{"-100"}, Body: "hi"})
	if err == nil || !strings.Contains(err.Error(), "retry after 30s") {
		t.Fatalf("err = %v", err)
	}
	if len(s.seen()) != 1 {
		t.Errorf("made %d calls; it retried", len(s.seen()))
	}
}

// The one path that cannot be atomic says so rather than being rounded either way.
func TestAFailedSecondCallIsPartial(t *testing.T) {
	s, ch, client := newStub(t)
	s.answer = func(method string) (int, string) {
		if strings.HasSuffix(method, "/sendMessage") {
			return 200, `{"ok":true,"result":{"message_id":1}}`
		}
		return 400, `{"ok":false,"description":"file too big"}`
	}
	res, err := send(t, s, ch, client, &channel.Message{
		To: []string{"-100"}, Body: strings.Repeat("x", maxCaption+1),
		Atts: []channel.Attachment{att("a.txt", "d")}})
	if err == nil {
		t.Fatal("the failed upload was not reported")
	}
	if res == nil || !res.Partial {
		t.Errorf("result = %+v, want Partial", res)
	}
}

// A single failed call is not partial: nothing was delivered.
func TestASingleFailedCallIsNotPartial(t *testing.T) {
	s, ch, client := newStub(t)
	s.answer = func(string) (int, string) { return 400, `{"ok":false,"description":"nope"}` }
	res, err := send(t, s, ch, client, &channel.Message{To: []string{"-100"}, Body: "hi"})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if res != nil && res.Partial {
		t.Error("a single failed call was reported as partial")
	}
}

func TestUnreachableAPI(t *testing.T) {
	ch := &config.Channel{Name: "alerts", Type: config.TypeTelegram, Token: "1:x",
		APIBase: "http://127.0.0.1:1", SendTimeout: config.Duration(config.DefaultSendTimeout)}
	_, err := New(ch, &http.Client{}).Send(context.Background(), &channel.Message{To: []string{"-100"}, Body: "hi"})
	if !errors.Is(err, channel.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestAttachmentContentIsUploaded(t *testing.T) {
	s, ch, client := newStub(t)
	if _, err := send(t, s, ch, client, &channel.Message{
		To: []string{"-100"}, Body: "hi", Atts: []channel.Attachment{att("report.csv", "1,2,3")}}); err != nil {
		t.Fatal(err)
	}
	if got := s.seen()[0].Files["document"]; got != "report.csv" {
		t.Errorf("uploaded filename = %q", got)
	}
}

// A CI notification is mostly a link, and a preview card over each one is noise.
func TestLinkPreview(t *testing.T) {
	t.Run("off sends link_preview_options", func(t *testing.T) {
		s, ch, client := newStub(t)
		if _, err := send(t, s, ch, client, &channel.Message{
			To: []string{"-100"}, Body: "see https://example.com", LinkPreview: false}); err != nil {
			t.Fatal(err)
		}
		got := s.seen()[0].Form.Get("link_preview_options")
		if got != `{"is_disabled":true}` {
			t.Errorf("link_preview_options = %q", got)
		}
		// The deprecated parameter is not also sent.
		if s.seen()[0].Form.Has("disable_web_page_preview") {
			t.Error("the deprecated parameter was sent too")
		}
	})

	t.Run("on sends nothing", func(t *testing.T) {
		s, ch, client := newStub(t)
		if _, err := send(t, s, ch, client, &channel.Message{
			To: []string{"-100"}, Body: "hi", LinkPreview: true}); err != nil {
			t.Fatal(err)
		}
		if s.seen()[0].Form.Has("link_preview_options") {
			t.Error("link_preview_options was sent when previews are on")
		}
	})
}
