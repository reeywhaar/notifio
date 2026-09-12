# Stack

Every dependency is a thing that can break, change under us, or need explaining to somebody
reading this in a year. This list is one line long.

| what | version | why |
| --- | --- | --- |
| Go | 1.27 | — |
| `net/http` | stdlib | The server, and the client that talks to Telegram |
| `net/smtp` | stdlib | The SMTP conversation |
| `mime/multipart`, `mime/quotedprintable`, `mime` | stdlib | Building a message and parsing a request. RFC 2047 and RFC 2231 come from here |
| `encoding/json` | stdlib | `config.json`, `data.json`, and the API |
| `log/slog` | stdlib | Structured logging without a dependency |
| `crypto/rand`, `crypto/sha256`, `crypto/subtle` | stdlib | Minting and checking a token — see [tokens.md](tokens.md#hashing) |
| `archive/tar`, `compress/gzip` | stdlib | The backup archive |
| `github.com/spf13/cobra` | v1.10.2 | Subcommands, so `docker exec notifio notifio token add x alerts` reads as what it does |
| `github.com/yuin/goldmark` | v1.8.6 | CommonMark, so `md` means one thing on every channel — see [channels.md](channels.md#markdown) |

**Two direct dependencies**, and the second one bought the removal of a whole class of bug:
without it `md` was MarkdownV2 passed through on Telegram and refused on email, which put
eighteen escaping rules on the caller and made one field mean two things.

## Not used, deliberately

- **No YAML parser.** `config.json` is JSON, which is what `data.json` already is and what a
  script writes without a library. The cost is that a config file cannot carry comments, which
  is why [channels.md](channels.md#every-key) documents every key.
- **No config interpolation.** No `${VAR}`. See [channels.md](channels.md#values-are-literal).
- **No Telegram bot library.** Three endpoints are used. Every such library brings the other
  hundred, plus an update loop notifio has no use for.
- **No mail library.** `net/smtp` plus `mime/multipart` is the whole message builder, and the
  parts that are actually hard — RFC 2047 subjects, RFC 2231 filenames, base64 line wrapping —
  are either stdlib or ten lines.
- **No HTML-to-text renderer.** It would exist to serve a plain-text fallback almost nothing
  reads. The Markdown renderer is a different case and is in.
- **No HTML sanitizer.** Rendering a caller's Markdown to HTML grants them nothing they did not
  already have: they hold a token and could have posted that HTML directly with
  `body_type: html`. Raw HTML inside `md` is dropped anyway, because Telegram refuses a tag it
  does not know.
- **No router.** Two exact paths and a catch-all, which is three lines of `http.ServeMux`.
- **No database, no queue, no scheduler.** A handful of rows in a JSON file, and a send whose
  result is its response.
- **No `golang.org/x/*` at all**, which is unusual enough to be worth writing down.
