package send

import (
	"strings"

	"notifio/internal/markup"
	"testing"
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

	t.Run("md on email renders", func(t *testing.T) {
		v, err := TestMessage(em, "", "s", BodyMD)
		if err != nil {
			t.Fatalf("md was refused on email: %v", err)
		}
		if !strings.Contains(v.Body, "<strong>notifio test</strong>") {
			t.Errorf("the markdown was not rendered:\n%s", v.Body)
		}
	})

	t.Run("an unknown body type is refused", func(t *testing.T) {
		if _, err := TestMessage(em, "", "s", "sgml"); err == nil {
			t.Error("the test command bypassed the validator")
		}
	})

	t.Run("a pinned channel still refuses --to", func(t *testing.T) {
		if _, err := TestMessage(tg, "-100elsewhere", "", BodyHTML); err == nil {
			t.Error("channel test redirected a pinned channel")
		}
	})
}

func TestEscapeHTML(t *testing.T) {
	if got := markup.EscapeHTML(`a<b>&"c"`); got != `a&lt;b&gt;&amp;"c"` {
		t.Errorf("EscapeHTML = %q", got)
	}
}

// md is CommonMark now, so the test message is written as ordinary markdown and rendered.
func TestTheMarkdownTestMessageRenders(t *testing.T) {
	ch := telegramCh()
	ch.Name = "alerts-2"
	ch.Pinned.To = []string{"-100"}

	v, err := TestMessage(ch, "", "", BodyMD)
	if err != nil {
		t.Fatal(err)
	}
	if v.Type != BodyHTML {
		t.Errorf("a sender was handed %q; only plain and html should reach one", v.Type)
	}
	for _, want := range []string{"<b>notifio test</b>", "<code>alerts-2</code>", "<a href="} {
		if !strings.Contains(v.Body, want) {
			t.Errorf("rendered body has no %s:\n%s", want, v.Body)
		}
	}
	if strings.Contains(v.Body, "**") || strings.Contains(v.Body, "- channel") {
		t.Errorf("the markdown was passed through unrendered:\n%s", v.Body)
	}
}
