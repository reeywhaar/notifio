package markup

import (
	"strings"
	"testing"
)

// Telegram accepts a short list of inline tags and no block elements at all. Anything else in
// the output fails the whole message, so the rule is: structure becomes whitespace.
func TestToTelegramHTML(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bold and italic", "**b** and *i*", "<b>b</b> and <i>i</i>"},
		{"code span", "a `x` b", "a <code>x</code> b"},
		{"link", "[text](https://example.com)", `<a href="https://example.com">text</a>`},
		{"autolink", "<https://example.com>", `<a href="https://example.com">https://example.com</a>`},
		{"heading becomes bold", "# Title\n\nbody", "<b>Title</b>\n\nbody"},
		{"paragraphs are blank-line separated", "one\n\ntwo", "one\n\ntwo"},
		{"bullets become bullets", "- a\n- b", "• a\n• b"},
		{"ordered list is numbered", "1. a\n2. b", "1. a\n2. b"},
		{"fenced code", "```\nx := 1\n```", "<pre>x := 1\n</pre>"},
		{"blockquote", "> quoted", "<blockquote>quoted</blockquote>"},
		{"thematic break", "a\n\n---\n\nb", "a\n\n———\n\nb"},
		{"image becomes a link", "![alt](https://x/i.png)", `<a href="https://x/i.png">alt</a>`},
		{"text is escaped", "5 < 6 & 7 > 2", "5 &lt; 6 &amp; 7 &gt; 2"},
		{"raw html tags go, their text stays", "a <div>b</div> c", "a b c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ToTelegramHTML(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("ToTelegramHTML(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// Every tag Telegram does not know fails the message, so none may appear.
func TestTelegramOutputCarriesNoBlockTags(t *testing.T) {
	const kitchenSink = `# Heading

Some **bold**, *italic*, ` + "`code`" + `, and [a link](https://example.com).

- one
- two
  - nested

1. first
2. second

> a quote

` + "```go\nfmt.Println(1)\n```" + `

---

![pic](https://x/i.png)
`
	got, err := ToTelegramHTML(kitchenSink)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<p>", "<br", "<ul>", "<ol>", "<li>", "<h1>", "<h2>", "<hr", "<img", "<div"} {
		if strings.Contains(got, bad) {
			t.Errorf("output carries %s, which Telegram refuses:\n%s", bad, got)
		}
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("output ends in a newline:\n%q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("output stacks blank lines:\n%q", got)
	}
}

func TestToHTML(t *testing.T) {
	got, err := ToHTML("**b** and [l](https://x)")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<p>", "<strong>b</strong>", `<a href="https://x">l</a>`} {
		if !strings.Contains(got, want) {
			t.Errorf("ToHTML has no %s:\n%s", want, got)
		}
	}
}

// A caller who wants HTML has body_type: html. Letting it through here would mean Telegram
// rejecting the message over a tag it does not know.
func TestRawHTMLIsNotPassedThrough(t *testing.T) {
	got, err := ToHTML("a <script>alert(1)</script> b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "<script>") {
		t.Errorf("raw html survived:\n%s", got)
	}
}
