// Package send parses a request, checks it against the type of channel its token was issued
// for, and hands it to that channel.
package send

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
)

// Body types. plain is the default; which of the others a type accepts is its own business.
const (
	BodyPlain = "plain"
	BodyMD    = "md"
	BodyHTML  = "html"
)

// spillToDisk is where multipart stops using memory. Small enough that an ordinary send costs
// no disk, large enough that an attachment is not held in the heap.
const spillToDisk = 1 << 20

// ErrRequest is anything wrong with what the caller sent.
var ErrRequest = errors.New("request")

// ErrLimit is a well-formed request the provider will not take.
var ErrLimit = errors.New("limit")

// Attachment is one file to send. Open is called at most once.
type Attachment struct {
	Filename    string
	ContentType string
	Size        int64

	open func() (io.ReadCloser, error)
}

// Open returns the content.
func (a Attachment) Open() (io.ReadCloser, error) { return a.open() }

// Request is one send, after parsing and before validation. Presence matters as much as value:
// a field that was sent empty is not a field that was absent.
type Request struct {
	To          []string
	From        string
	Subject     string
	Body        string
	BodyType    string
	LinkPreview bool
	Channel     string // only ever set by a caller that should not have; refused in validate
	Atts        []Attachment
	cleanup     func()
	present     map[string]bool
	multipart   bool
}

// Cleanup removes anything multipart spilled to disk. Safe to call more than once.
func (r *Request) Cleanup() {
	if r.cleanup != nil {
		r.cleanup()
		r.cleanup = nil
	}
}

func (r *Request) has(field string) bool { return r.present[field] }

// filenameBad is what a filename may not contain once it is down to a base name: path
// separators, the characters a Windows path reserves, and controls.
//
// It is a denylist rather than an allowlist of ASCII, because the alphabet is not the hazard —
// `отчёт.csv` is a perfectly good name, and both the MIME header (RFC 2231) and the Telegram
// upload carry it.
var filenameBad = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f\x7f]`)

// safeFilename reduces a filename to a label. It reaches a temp file, a MIME header and a
// multipart body, and a path separator means something different in each.
func safeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = filenameBad.ReplaceAllString(name, "_")
	name = strings.Trim(name, " .")
	// Runes, not bytes: cutting at a byte boundary can split a multi-byte character and
	// produce a name that is not valid UTF-8.
	if r := []rune(name); len(r) > 128 {
		name = string(r[:128])
	}
	return name
}

// Parse reads a request body. The Content-Type decides the parser and nothing else.
func Parse(r *http.Request, maxBody int64) (*Request, error) {
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, fmt.Errorf("%w: Content-Type %q: %v", ErrUnsupportedMedia, ct, err)
	}

	switch mediaType {
	case "application/json":
		return parseJSON(r.Body)
	case "multipart/form-data":
		return parseMultipart(r, params["boundary"], maxBody)
	case "application/x-www-form-urlencoded":
		return parseURLEncoded(r)
	default:
		return nil, fmt.Errorf("%w: %q is not application/json, multipart/form-data or application/x-www-form-urlencoded", ErrUnsupportedMedia, mediaType)
	}
}

// ErrUnsupportedMedia is a Content-Type notifio does not parse.
var ErrUnsupportedMedia = errors.New("unsupported media type")

type jsonAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
}

func parseJSON(body io.Reader) (*Request, error) {
	// Pointers so "absent" and "sent empty" stay distinguishable.
	var in struct {
		To          json.RawMessage  `json:"to"`
		From        *string          `json:"from"`
		Subject     *string          `json:"subject"`
		Body        *string          `json:"body"`
		BodyType    *string          `json:"body_type"`
		LinkPreview *bool            `json:"link_preview"`
		Channel     *string          `json:"channel"`
		Attachments []jsonAttachment `json:"attachments"`
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequest, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing content after the JSON object", ErrRequest)
	}

	req := &Request{present: map[string]bool{}}
	if in.To != nil {
		req.present["to"] = true
		to, err := decodeTo(in.To)
		if err != nil {
			return nil, err
		}
		req.To = to
	}
	setStr(req, "from", in.From, &req.From)
	setStr(req, "subject", in.Subject, &req.Subject)
	setStr(req, "body", in.Body, &req.Body)
	setStr(req, "body_type", in.BodyType, &req.BodyType)
	setStr(req, "channel", in.Channel, &req.Channel)
	if in.LinkPreview != nil {
		req.present["link_preview"] = true
		req.LinkPreview = *in.LinkPreview
	}

	for i, a := range in.Attachments {
		req.present["attachments"] = true
		name := safeFilename(a.Filename)
		if name == "" {
			return nil, fmt.Errorf("%w: attachment %d has no usable filename", ErrRequest, i)
		}
		raw, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("%w: attachment %q: data is not base64: %v", ErrRequest, name, err)
		}
		ctype := a.ContentType
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		req.Atts = append(req.Atts, Attachment{
			Filename:    name,
			ContentType: ctype,
			Size:        int64(len(raw)),
			open:        func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(string(raw))), nil },
		})
	}
	return req, nil
}

// parseBool is strict: a value outside the two vocabularies is refused rather than read as
// false, because a typo in a flag that turns something off must not turn it on.
func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%w: link_preview %q is not true or false", ErrRequest, v)
}

func setStr(req *Request, field string, in *string, out *string) {
	if in != nil {
		req.present[field] = true
		*out = *in
	}
}

// decodeTo accepts one address or a list of them.
func decodeTo(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf(`%w: "to" is a string or an array of strings`, ErrRequest)
	}
	return many, nil
}

func parseURLEncoded(r *http.Request) (*Request, error) {
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequest, err)
	}
	if _, ok := r.PostForm["attachments"]; ok {
		return nil, fmt.Errorf("%w: urlencoded requests cannot carry attachments; use multipart/form-data", ErrRequest)
	}
	return fromValues(r.PostForm, nil)
}

func parseMultipart(r *http.Request, boundary string, maxBody int64) (*Request, error) {
	if boundary == "" {
		return nil, fmt.Errorf("%w: multipart/form-data with no boundary", ErrRequest)
	}
	if err := r.ParseMultipartForm(spillToDisk); err != nil {
		if strings.Contains(err.Error(), "too large") {
			return nil, ErrTooLarge
		}
		return nil, fmt.Errorf("%w: %v", ErrRequest, err)
	}
	form := r.MultipartForm
	req, err := fromValues(form.Value, form.File)
	if req != nil {
		req.multipart = true
		req.cleanup = func() { form.RemoveAll() }
	}
	if err != nil {
		if req != nil {
			req.Cleanup()
		}
		return nil, err
	}
	return req, nil
}

// ErrTooLarge is a body over the channel's max_body.
var ErrTooLarge = errors.New("request body too large")

// scalars are the fields that may appear at most once. Repeating one has a first-wins and a
// last-wins answer, and either produces a message somebody did not write.
var scalars = []string{"from", "subject", "body", "body_type", "channel", "link_preview"}

func fromValues(values map[string][]string, files map[string][]*multipart.FileHeader) (*Request, error) {
	req := &Request{present: map[string]bool{}}

	for _, f := range scalars {
		v, ok := values[f]
		if !ok {
			continue
		}
		if len(v) > 1 {
			return nil, fmt.Errorf("%w: %q was given %d times", ErrRequest, f, len(v))
		}
		req.present[f] = true
		switch f {
		case "from":
			req.From = v[0]
		case "subject":
			req.Subject = v[0]
		case "body":
			req.Body = v[0]
		case "body_type":
			req.BodyType = v[0]
		case "channel":
			req.Channel = v[0]
		case "link_preview":
			b, err := parseBool(v[0])
			if err != nil {
				return nil, err
			}
			req.LinkPreview = b
		}
	}
	if v, ok := values["to"]; ok {
		req.present["to"] = true
		req.To = v
	}

	for _, fh := range files["attachments"] {
		req.present["attachments"] = true
		name := safeFilename(fh.Filename)
		if name == "" {
			return req, fmt.Errorf("%w: an attachment has no usable filename", ErrRequest)
		}
		ctype := fh.Header.Get("Content-Type")
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		req.Atts = append(req.Atts, Attachment{
			Filename:    name,
			ContentType: ctype,
			Size:        fh.Size,
			open:        func() (io.ReadCloser, error) { return fh.Open() },
		})
	}
	return req, nil
}
