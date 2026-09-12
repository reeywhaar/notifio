package send

import (
	"strings"
	"testing"
	"time"
)

func TestTestMessageIsMarkedUp(t *testing.T) {
	tg, em := telegramCh(), emailCh()
	tg.Pinned.To = []string{"-100"}
	em.Pinned.From = "n@x.com"
	em.Pinned.To = []string{"ops@x.com"}

	t.Run("telegram html has no br", func(t *testing.T) {
		v, err := TestMessage(tg, "", "", BodyHTML)
		if err != nil {
			t.Fatal(err)
		}
		// Telegram's HTML subset rejects an unsupported start tag, and <br> is one.
		if strings.Contains(v.Body, "<br") || strings.Contains(v.Body, "<p>") {
			t.Errorf("telegram body carries a tag Telegram refuses:\n%s", v.Body)
		}
		for _, tag := range []string{"<b>", "<code>", "<a href="} {
			if !strings.Contains(v.Body, tag) {
				t.Errorf("telegram body has no %s", tag)
			}
		}
	})

	t.Run("email html uses br", func(t *testing.T) {
		v, err := TestMessage(em, "", "notifio test", BodyHTML)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.Body, "<br>") {
			t.Errorf("email body relies on newlines, which HTML collapses:\n%s", v.Body)
		}
	})

	t.Run("plain carries no markup", func(t *testing.T) {
		v, err := TestMessage(tg, "", "", BodyPlain)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(v.Body, "<>") {
			t.Errorf("plain body carries markup:\n%s", v.Body)
		}
	})

	t.Run("md on email is refused", func(t *testing.T) {
		if _, err := TestMessage(em, "", "s", BodyMD); err == nil {
			t.Error("the test command bypassed the validator")
		}
	})

	t.Run("a pinned channel still refuses --to", func(t *testing.T) {
		if _, err := TestMessage(tg, "-100elsewhere", "", BodyHTML); err == nil {
			t.Error("channel test redirected a pinned channel")
		}
	})
}

// notifio escapes MarkdownV2 for nobody, including itself, so its own test message has to be
// correct — which is most of what sending one proves.
func TestTheMarkdownTestMessageEscapesItsDynamicParts(t *testing.T) {
	ch := telegramCh()
	ch.Name = "alerts-2"
	ch.Pinned.To = []string{"-100"}

	body := testBody(ch, BodyMD, time.Date(2026, 9, 12, 4, 5, 6, 0, time.UTC))

	// The timestamp is the part that would break a real send: it carries - and . and :
	if !strings.Contains(body, `2026\-09\-12`) {
		t.Errorf("the timestamp's hyphens are unescaped:\n%s", body)
	}
	// The link label, where a bare . ends the message with a 400.
	if !strings.Contains(body, `github\.com`) {
		t.Errorf("the link label's dots are unescaped:\n%s", body)
	}
	// ...but not the URL inside (), where escaping would corrupt it.
	if !strings.Contains(body, "("+"https://github.com/reeywhaar/notifio)") {
		t.Errorf("the link target was escaped and is now wrong:\n%s", body)
	}
	if !strings.Contains(body, "*notifio test*") {
		t.Error("the markdown body carries no bold")
	}
	// The channel name sits in a code span, where only ` and \ are reserved — and a channel
	// name can hold neither, so alerts-2 goes in as it is.
	if !strings.Contains(body, "`alerts-2`") {
		t.Errorf("the channel name was mangled:\n%s", body)
	}
}

func TestEscapeMarkdownV2(t *testing.T) {
	for _, r := range markdownV2Special {
		in := "a" + string(r) + "b"
		want := "a\\" + string(r) + "b"
		if got := escapeMarkdownV2(in); got != want {
			t.Errorf("escapeMarkdownV2(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escapeMarkdownV2("plain text 123"); got != "plain text 123" {
		t.Errorf("escaped something it should not have: %q", got)
	}
	if got := escapeMarkdownV2("Дом"); got != "Дом" {
		t.Errorf("mangled non-ASCII: %q", got)
	}
}

func TestEscapeHTML(t *testing.T) {
	if got := escapeHTML(`a<b>&"c"`); got != `a&lt;b&gt;&amp;"c"` {
		t.Errorf("escapeHTML = %q", got)
	}
}
