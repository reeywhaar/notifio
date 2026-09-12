package send

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"notifio/internal/config"
)

func telegramCh() *config.Channel {
	return &config.Channel{Name: "alerts", Type: config.TypeTelegram, Token: "1:x",
		APIBase: config.DefaultAPIBase, MaxBody: config.DefaultMaxBody,
		SendTimeout: config.Duration(config.DefaultSendTimeout)}
}

func emailCh() *config.Channel {
	return &config.Channel{Name: "notices", Type: config.TypeEmail, Host: "smtp.example.com",
		Port: 587, Encryption: config.EncStartTLS, MaxBody: config.DefaultMaxBody,
		SendTimeout: config.Duration(config.DefaultSendTimeout)}
}

func parseForm(t *testing.T, v url.Values) *Request {
	t.Helper()
	r, err := Parse(formReq(t, v), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The token picks the validator. A field the type has no use for is refused, never ignored.
func TestRefusedWhereItHasNoMeaning(t *testing.T) {
	cases := []struct {
		name string
		ch   *config.Channel
		v    url.Values
		want string
	}{
		{"subject on telegram", telegramCh(), url.Values{"to": {"5"}, "body": {"x"}, "subject": {"hi"}}, `no "subject"`},
		{"from on telegram", telegramCh(), url.Values{"to": {"5"}, "body": {"x"}, "from": {"a@x.com"}}, `no "from"`},
		{"md on email", emailCh(), url.Values{"to": {"a@x.com"}, "from": {"b@x.com"}, "subject": {"s"}, "body": {"x"}, "body_type": {"md"}}, "body_type"},
		{"channel on telegram", telegramCh(), url.Values{"to": {"5"}, "body": {"x"}, "channel": {"alerts"}}, `no "channel" field`},
		{"channel on email", emailCh(), url.Values{"to": {"a@x.com"}, "from": {"b@x.com"}, "subject": {"s"}, "body": {"x"}, "channel": {"notices"}}, `no "channel" field`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(parseForm(t, c.v), c.ch)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// The one field whose cardinality differs by type.
func TestToCardinality(t *testing.T) {
	_, err := Validate(parseForm(t, url.Values{"to": {"5", "6"}, "body": {"x"}}), telegramCh())
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("telegram accepted two recipients: %v", err)
	}

	v, err := Validate(parseForm(t, url.Values{
		"to": {"a@x.com", "b@x.com"}, "from": {"c@x.com"}, "subject": {"s"}, "body": {"x"}}), emailCh())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.To) != 2 {
		t.Errorf("email kept %d recipients, want 2", len(v.To))
	}
}

func TestDuplicateRecipientsCollapse(t *testing.T) {
	v, err := Validate(parseForm(t, url.Values{
		"to": {"a@x.com", "A@x.com ", "a@x.com", "b@x.com"}, "from": {"c@x.com"},
		"subject": {"s"}, "body": {"x"}}), emailCh())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.To) != 3 {
		t.Errorf("to = %v; only exact duplicates collapse", v.To)
	}
}

// A failed interpolation must not be accepted as an absence.
func TestPresentButEmptyIsNotAbsent(t *testing.T) {
	if _, err := Validate(parseForm(t, url.Values{"to": {""}, "body": {"x"}}), telegramCh()); err == nil {
		t.Error(`to="" was accepted`)
	}
	if _, err := Validate(parseForm(t, url.Values{
		"to": {""}, "from": {"c@x.com"}, "subject": {"s"}, "body": {"x"}}), emailCh()); err == nil {
		t.Error(`to="" was accepted on email`)
	}

	if _, err := Validate(parseForm(t, url.Values{"to": {"5"}, "body": {""}}), telegramCh()); err == nil {
		t.Error(`body="" was accepted`)
	}
	if _, err := Validate(parseForm(t, url.Values{"to": {"5"}, "body": {"x"}, "body_type": {""}}), telegramCh()); err == nil {
		t.Error(`body_type="" was accepted`)
	}
}

// A telegram channel with a chat_id is a fixed destination, and a request cannot redirect it.
func TestAPinnedChannelRefusesTo(t *testing.T) {
	ch := telegramCh()
	ch.Pinned.To = []string{"-100pinned"}

	v, err := Validate(parseForm(t, url.Values{"body": {"x"}}), ch)
	if err != nil {
		t.Fatal(err)
	}
	if v.To[0] != "-100pinned" {
		t.Errorf("to = %v, want the pinned chat", v.To)
	}

	// Contradicting the pin is refused: a caller told their message went to one place while it
	// went to another has been lied to with a 200.
	_, err = Validate(parseForm(t, url.Values{"to": {"-100elsewhere"}, "body": {"x"}}), ch)
	if err == nil {
		t.Fatal("a pinned channel was redirected by the request")
	}
	if !strings.Contains(err.Error(), "pins") || !strings.Contains(err.Error(), "-100pinned") {
		t.Errorf("error = %q, want it to name the field and what it is pinned to", err)
	}

	// Stating it is not contradicting it, so a caller that documents its own destination keeps
	// working.
	v, err = Validate(parseForm(t, url.Values{"to": {"-100pinned"}, "body": {"x"}}), ch)
	if err != nil {
		t.Fatalf("a `to` naming the pinned chat was refused: %v", err)
	}
	if v.To[0] != "-100pinned" {
		t.Errorf("to = %v", v.To)
	}
	// Whitespace is not a difference.
	if _, err := Validate(parseForm(t, url.Values{"to": {" -100pinned "}, "body": {"x"}}), ch); err != nil {
		t.Errorf("a padded but identical value was refused: %v", err)
	}
}

// An email channel pins a list, and order is not a difference.
func TestAPinnedRecipientListMayBeRestated(t *testing.T) {
	ch := emailCh()
	ch.Pinned.From = "n@x.com"
	ch.Pinned.To = []string{"ops@x.com", "oncall@x.com"}
	base := url.Values{"subject": {"s"}, "body": {"x"}}

	restate := func(to ...string) error {
		v := url.Values{"subject": {"s"}, "body": {"x"}}
		v["to"] = to
		_, err := Validate(parseForm(t, v), ch)
		return err
	}
	if _, err := Validate(parseForm(t, base), ch); err != nil {
		t.Fatalf("a body-only request was refused: %v", err)
	}
	if err := restate("oncall@x.com", "ops@x.com"); err != nil {
		t.Errorf("the same two recipients in another order were refused: %v", err)
	}
	if err := restate("ops@x.com"); err == nil {
		t.Error("a subset of the pinned recipients was accepted")
	}
	if err := restate("ops@x.com", "oncall@x.com", "extra@x.com"); err == nil {
		t.Error("an added recipient was accepted")
	}
	// A display name is a different value, and the rule is textual on purpose.
	if err := restate("Ops <ops@x.com>", "oncall@x.com"); err == nil {
		t.Error("a rewritten recipient was accepted")
	}
}

// Without a chat_id the channel is open, and every request names its own chat.
func TestAnOpenChannelRequiresTo(t *testing.T) {
	ch := telegramCh()
	if _, err := Validate(parseForm(t, url.Values{"body": {"x"}}), ch); err == nil {
		t.Error("an open channel accepted a request with no `to`")
	}
	v, err := Validate(parseForm(t, url.Values{"to": {"-100chosen"}, "body": {"x"}}), ch)
	if err != nil {
		t.Fatal(err)
	}
	if v.To[0] != "-100chosen" {
		t.Errorf("to = %v", v.To)
	}
}

// `from` stays a fallback, and the asymmetry with chat_id is deliberate: `from` is who the
// message is from, not where it goes, so overriding it redirects nothing.
func TestEmailFromFallback(t *testing.T) {
	ch := emailCh()
	ch.Pinned.From = "Notifio <n@x.com>"
	v, err := Validate(parseForm(t, url.Values{"to": {"a@x.com"}, "subject": {"s"}, "body": {"x"}}), ch)
	if err != nil {
		t.Fatal(err)
	}
	if v.From != ch.Pinned.From {
		t.Errorf("from = %q", v.From)
	}
	if _, err := Validate(parseForm(t, url.Values{"to": {"a@x.com"}, "subject": {"s"}, "body": {"x"}}), emailCh()); err == nil {
		t.Error("no from anywhere, and it was accepted")
	}
}

// "Doe, Jane" <jane@x> is one address containing a comma, so commas are not split on.
func TestCommasAreNotSplitOn(t *testing.T) {
	_, err := Validate(parseForm(t, url.Values{
		"to": {"a@x.com, b@x.com"}, "from": {"c@x.com"}, "subject": {"s"}, "body": {"x"}}), emailCh())
	if err == nil || !strings.Contains(err.Error(), "holds 2 addresses") {
		t.Fatalf("a comma-separated list was accepted: %v", err)
	}

	v, err := Validate(parseForm(t, url.Values{
		"to": {`"Doe, Jane" <jane@x.com>`}, "from": {"c@x.com"}, "subject": {"s"}, "body": {"x"}}), emailCh())
	if err != nil {
		t.Fatalf("a quoted name with a comma was refused: %v", err)
	}
	if len(v.To) != 1 {
		t.Errorf("to = %v, want one recipient", v.To)
	}
}

// The input that turns a notifier into an open relay.
func TestHeaderInjectionIsRefused(t *testing.T) {
	for _, field := range []string{"subject", "to", "from"} {
		v := url.Values{"to": {"a@x.com"}, "from": {"b@x.com"}, "subject": {"s"}, "body": {"x"}}
		v.Set(field, v.Get(field)+"\r\nBcc: evil@example.com")
		if _, err := Validate(parseForm(t, v), emailCh()); err == nil {
			t.Errorf("a line break in %q was accepted", field)
		}
	}
}

func TestSubjectIsRequiredAndNonEmpty(t *testing.T) {
	base := url.Values{"to": {"a@x.com"}, "from": {"b@x.com"}, "body": {"x"}}
	if _, err := Validate(parseForm(t, base), emailCh()); err == nil {
		t.Error("a missing subject was accepted")
	}
	withBlank := url.Values{"to": {"a@x.com"}, "from": {"b@x.com"}, "body": {"x"}, "subject": {"   "}}
	if _, err := Validate(parseForm(t, withBlank), emailCh()); err == nil {
		t.Error("a whitespace subject was accepted")
	}
}

func TestBodyTypeDefaults(t *testing.T) {
	v, err := Validate(parseForm(t, url.Values{"to": {"5"}, "body": {"x"}}), telegramCh())
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != BodyPlain {
		t.Errorf("body_type = %q, want %q", v.Type, BodyPlain)
	}
	for _, bt := range []string{BodyMD, BodyHTML} {
		if _, err := Validate(parseForm(t, url.Values{"to": {"5"}, "body": {"x"}, "body_type": {bt}}), telegramCh()); err != nil {
			t.Errorf("telegram refused body_type %q: %v", bt, err)
		}
	}
}

// Refused rather than truncated or split.
func TestTelegramLimits(t *testing.T) {
	long := strings.Repeat("a", TelegramMaxText+1)
	_, err := Validate(parseForm(t, url.Values{"to": {"5"}, "body": {long}}), telegramCh())
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("a %d-character body = %v, want ErrLimit", len(long), err)
	}

	req := parseForm(t, url.Values{"to": {"5"}, "body": {"x"}})
	for range TelegramMaxAlbum + 1 {
		req.Atts = append(req.Atts, Attachment{Filename: "a.txt"})
	}
	if _, err := Validate(req, telegramCh()); !errors.Is(err, ErrLimit) {
		t.Fatalf("%d attachments = %v, want ErrLimit", TelegramMaxAlbum+1, err)
	}
}

// A caption-length body with attachments is fine; the sender splits it into two calls.
func TestALongBodyWithAttachmentsIsAllowed(t *testing.T) {
	req := parseForm(t, url.Values{"to": {"5"}, "body": {strings.Repeat("b", TelegramMaxCaption+100)}})
	req.Atts = []Attachment{{Filename: "a.txt"}}
	if _, err := Validate(req, telegramCh()); err != nil {
		t.Fatalf("refused a body the sender would split: %v", err)
	}
}

func TestLinkPreview(t *testing.T) {
	base := url.Values{"to": {"-100"}, "body": {"x"}}

	t.Run("defaults to on", func(t *testing.T) {
		v, err := Validate(parseForm(t, base), telegramCh())
		if err != nil {
			t.Fatal(err)
		}
		if !v.LinkPreview {
			t.Error("link_preview defaulted to off; Telegram's own default is on")
		}
	})

	t.Run("a request can turn it off", func(t *testing.T) {
		v, err := Validate(parseForm(t, url.Values{
			"to": {"-100"}, "body": {"x"}, "link_preview": {"false"}}), telegramCh())
		if err != nil {
			t.Fatal(err)
		}
		if v.LinkPreview {
			t.Error("link_preview=false was ignored")
		}
	})

	// A typo in a flag that turns something off must not turn it on. Refused by the parser, so
	// it fails the same way on every content type.
	t.Run("an unrecognised value is refused", func(t *testing.T) {
		for _, bad := range []string{"flase", "", "maybe", "2"} {
			v := url.Values{"to": {"-100"}, "body": {"x"}, "link_preview": {bad}}
			if _, err := Parse(formReq(t, v), 1<<20); err == nil {
				t.Errorf("link_preview=%q was accepted", bad)
			}
		}
	})

	t.Run("pinned refuses a request value", func(t *testing.T) {
		ch := telegramCh()
		off := false
		ch.Pinned.LinkPreview = &off

		v, err := Validate(parseForm(t, base), ch)
		if err != nil {
			t.Fatal(err)
		}
		if v.LinkPreview {
			t.Error("the pinned value was not used")
		}
		// Restating the pinned value is fine; contradicting it is not.
		if _, err := Validate(parseForm(t, url.Values{
			"to": {"-100"}, "body": {"x"}, "link_preview": {"false"}}), ch); err != nil {
			t.Errorf("a link_preview matching the pin was refused: %v", err)
		}
		if _, err := Validate(parseForm(t, url.Values{
			"to": {"-100"}, "body": {"x"}, "link_preview": {"true"}}), ch); err == nil {
			t.Error("a link_preview contradicting the pin was accepted")
		}
	})

	t.Run("refused on email", func(t *testing.T) {
		v := url.Values{"to": {"a@x.com"}, "from": {"b@x.com"}, "subject": {"s"},
			"body": {"x"}, "link_preview": {"false"}}
		if _, err := Validate(parseForm(t, v), emailCh()); err == nil {
			t.Error("an email channel accepted link_preview")
		}
	})
}
