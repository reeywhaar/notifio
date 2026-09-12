package send

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func jsonReq(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func formReq(t *testing.T, values url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

type file struct{ field, name, ctype, body string }

func multipartReq(t *testing.T, values url.Values, files []file) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range values {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="` + f.field + `"; filename="` + f.name + `"`}
		if f.ctype != "" {
			h["Content-Type"] = []string{f.ctype}
		}
		p, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(p, f.body)
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/send", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// The three parsers must not drift apart.
func TestTheThreeContentTypesAgree(t *testing.T) {
	want := struct {
		to, subject, body, bodyType string
	}{"ops@example.com", "Disk at 91%", "it is full", "html"}

	values := url.Values{
		"to": {want.to}, "subject": {want.subject}, "body": {want.body}, "body_type": {want.bodyType},
	}
	reqs := map[string]*http.Request{
		"json":       jsonReq(t, `{"to":"ops@example.com","subject":"Disk at 91%","body":"it is full","body_type":"html"}`),
		"urlencoded": formReq(t, values),
		"multipart":  multipartReq(t, values, nil),
	}
	for name, r := range reqs {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(r, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer got.Cleanup()
			if len(got.To) != 1 || got.To[0] != want.to {
				t.Errorf("to = %v", got.To)
			}
			if got.Subject != want.subject || got.Body != want.body || got.BodyType != want.bodyType {
				t.Errorf("got %+v", got)
			}
			for _, f := range []string{"to", "subject", "body", "body_type"} {
				if !got.has(f) {
					t.Errorf("%q not recorded as present", f)
				}
			}
			if got.has("from") || got.has("channel") {
				t.Error("a field that was not sent is recorded as present")
			}
		})
	}
}

// Repeating a scalar has a first-wins and a last-wins answer, and either sends a message
// somebody did not write.
func TestARepeatedScalarIsRefused(t *testing.T) {
	r := formReq(t, url.Values{"body": {"one", "two"}, "to": {"a@x.com"}})
	if _, err := Parse(r, 1<<20); err == nil {
		t.Fatal("a repeated body was accepted")
	}
}

func TestToMayRepeat(t *testing.T) {
	r := formReq(t, url.Values{"body": {"x"}, "to": {"a@x.com", "b@x.com"}})
	got, err := Parse(r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.To) != 2 {
		t.Fatalf("to = %v, want both", got.To)
	}
}

func TestUnknownJSONFieldIsRefused(t *testing.T) {
	if _, err := Parse(jsonReq(t, `{"body":"x","chanel":"alerts"}`), 1<<20); err == nil {
		t.Fatal("a misspelled field was accepted")
	}
}

func TestUnsupportedContentType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader("x"))
	r.Header.Set("Content-Type", "text/plain")
	_, err := Parse(r, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "unsupported media type") {
		t.Fatalf("err = %v", err)
	}
}

func TestURLEncodedCannotCarryAttachments(t *testing.T) {
	r := formReq(t, url.Values{"body": {"x"}, "attachments": {"nope"}})
	if _, err := Parse(r, 1<<20); err == nil {
		t.Fatal("an attachments field on a urlencoded request was accepted")
	}
}

// A filename reaches a temp file, a MIME header and a multipart body, and a path separator
// means something different in each.
func TestFilenameIsReducedToALabel(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":      "passwd",
		`..\..\windows\sys.ini`: "sys.ini",
		"report 2026.csv":       "report 2026.csv",
		"naughty;rm -rf.txt":    "naughty;rm -rf.txt",
		// The alphabet is not the hazard: RFC 2231 carries these, and stripping them would
		// make every non-Latin attachment arrive as underscores.
		"отчёт.csv":     "отчёт.csv",
		"日本語.txt":       "日本語.txt",
		"a:b*c?.txt":    "a_b_c_.txt",
		"tab\there.txt": "tab_here.txt",
	}
	for in, want := range cases {
		r := multipartReq(t, url.Values{"body": {"x"}}, []file{{"attachments", in, "text/plain", "data"}})
		got, err := Parse(r, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer got.Cleanup()
		if len(got.Atts) != 1 || got.Atts[0].Filename != want {
			t.Errorf("filename %q became %q, want %q", in, got.Atts[0].Filename, want)
		}
	}
}

func TestAttachmentWithNoUsableFilename(t *testing.T) {
	r := multipartReq(t, url.Values{"body": {"x"}}, []file{{"attachments", "...", "text/plain", "d"}})
	if _, err := Parse(r, 1<<20); err == nil {
		t.Fatal("an attachment with no usable name was accepted")
	}
}

func TestJSONAttachmentIsBase64(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"body": "x",
		"attachments": []map[string]string{
			{"filename": "a.txt", "content_type": "text/plain", "data": base64.StdEncoding.EncodeToString([]byte("hello"))},
		},
	})
	got, err := Parse(jsonReq(t, string(body)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := got.Atts[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Errorf("content = %q", data)
	}

	if _, err := Parse(jsonReq(t, `{"body":"x","attachments":[{"filename":"a","data":"!!not base64!!"}]}`), 1<<20); err == nil {
		t.Fatal("non-base64 data was accepted")
	}
}

func TestAttachmentDefaultsToOctetStream(t *testing.T) {
	r := multipartReq(t, url.Values{"body": {"x"}}, []file{{"attachments", "a.bin", "", "d"}})
	got, _ := Parse(r, 1<<20)
	defer got.Cleanup()
	if got.Atts[0].ContentType != "application/octet-stream" {
		t.Errorf("content type = %q", got.Atts[0].ContentType)
	}
}
