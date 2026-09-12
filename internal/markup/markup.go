// Package markup renders CommonMark, so that `md` means one thing on every channel.
//
// The alternative was passing MarkdownV2 through to Telegram and refusing it on email, which
// made the same field mean two things and put eighteen escaping rules on the caller. Here a
// caller writes ordinary Markdown and notifio produces whatever each provider can read.
package markup

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// md is the parser both renderers share, so they always see the same document.
//
// Raw HTML in the source is not passed through: a caller who wants HTML has `body_type: html`,
// and letting it through here would mean Telegram rejecting the message over a tag it does not
// know.
var md = goldmark.New()

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeHTML escapes text for both HTML and Telegram's subset of it.
func EscapeHTML(s string) string { return htmlEscaper.Replace(s) }

// ToHTML renders CommonMark as ordinary HTML, for an email body.
func ToHTML(source string) (string, error) {
	var buf bytes.Buffer
	if err := md.Convert([]byte(source), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ToTelegramHTML renders CommonMark into the subset of HTML the Bot API accepts.
//
// Telegram has no block elements: no <p>, no <br>, no lists, no headings. Structure has to
// become whitespace and the few inline tags it does know, which is what this renderer does.
func ToTelegramHTML(source string) (string, error) {
	doc := md.Parser().Parse(text.NewReader([]byte(source)))
	var buf bytes.Buffer
	r := &telegram{src: []byte(source), out: &buf}
	if err := ast.Walk(doc, r.walk); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

type telegram struct {
	src  []byte
	out  *bytes.Buffer
	list []listState
}

type listState struct {
	ordered bool
	n       int
}

func (t *telegram) write(s string) { t.out.WriteString(s) }

// blank ends the current block with one empty line, without stacking them up.
func (t *telegram) blank() {
	if t.out.Len() == 0 {
		return
	}
	t.trim()
	t.out.WriteString("\n\n")
}

// trim drops trailing newlines, so a closing tag lands against its content.
func (t *telegram) trim() {
	trimmed := strings.TrimRight(t.out.String(), "\n")
	t.out.Reset()
	t.out.WriteString(trimmed)
}

func (t *telegram) walk(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch n := n.(type) {
	case *ast.Document:

	case *ast.Heading:
		// Telegram has no headings. Bold is what a heading looks like there.
		if entering {
			t.write("<b>")
		} else {
			t.write("</b>")
			t.blank()
		}

	case *ast.Paragraph:
		if !entering {
			if t.inList() {
				t.write("\n")
			} else {
				t.blank()
			}
		}

	case *ast.TextBlock:
		if !entering && t.inList() {
			t.write("\n")
		}

	case *ast.Blockquote:
		if entering {
			t.write("<blockquote>")
		} else {
			// The paragraph inside has already ended itself with a blank line, and a closing
			// tag has to land against its content.
			t.trim()
			t.write("</blockquote>")
			t.blank()
		}

	case *ast.List:
		if entering {
			t.list = append(t.list, listState{ordered: n.IsOrdered(), n: n.Start})
		} else {
			t.list = t.list[:len(t.list)-1]
			if !t.inList() {
				t.blank()
			}
		}

	case *ast.ListItem:
		if entering {
			t.write(strings.Repeat("  ", len(t.list)-1))
			s := &t.list[len(t.list)-1]
			if s.ordered {
				t.write(strconv.Itoa(s.n) + ". ")
				s.n++
			} else {
				t.write("• ")
			}
		}

	case *ast.ThematicBreak:
		if !entering {
			t.write("———")
			t.blank()
		}

	case *ast.Emphasis:
		tag := "i"
		if n.Level >= 2 {
			tag = "b"
		}
		if entering {
			t.write("<" + tag + ">")
		} else {
			t.write("</" + tag + ">")
		}

	case *ast.Link:
		if entering {
			t.write(`<a href="` + EscapeHTML(string(n.Destination)) + `">`)
		} else {
			t.write("</a>")
		}

	case *ast.AutoLink:
		if entering {
			u := string(n.URL(t.src))
			t.write(`<a href="` + EscapeHTML(u) + `">` + EscapeHTML(string(n.Label(t.src))) + "</a>")
		}

	case *ast.Image:
		// Telegram will not inline an image from message text, so it becomes a link to one.
		if entering {
			t.write(`<a href="` + EscapeHTML(string(n.Destination)) + `">`)
		} else {
			t.write("</a>")
		}

	case *ast.CodeSpan:
		if entering {
			t.write("<code>")
		} else {
			t.write("</code>")
		}

	case *ast.FencedCodeBlock, *ast.CodeBlock:
		if entering {
			t.write("<pre>")
			t.writeLines(n)
			t.write("</pre>")
			t.blank()
		}
		return ast.WalkSkipChildren, nil

	case *ast.Text:
		if entering {
			t.write(EscapeHTML(string(n.Segment.Value(t.src))))
			if n.HardLineBreak() || n.SoftLineBreak() {
				t.write("\n")
			}
		}

	case *ast.String:
		if entering {
			t.write(EscapeHTML(string(n.Value)))
		}

	case *ast.RawHTML, *ast.HTMLBlock:
		// Dropped rather than passed on: a tag Telegram does not know fails the whole message.
		return ast.WalkSkipChildren, nil
	}
	return ast.WalkContinue, nil
}

func (t *telegram) inList() bool { return len(t.list) > 0 }

func (t *telegram) writeLines(n ast.Node) {
	l := n.Lines()
	for i := range l.Len() {
		line := l.At(i)
		t.write(EscapeHTML(string(line.Value(t.src))))
	}
}
