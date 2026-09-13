# Conventions

## Contents

- [Naming things](#naming-things)
- [Commit messages](#commit-messages)
- [Comments](#comments)
- [Go](#go)
  - [Tests](#tests)
- [Time](#time)

## Naming things

| word | meaning |
| --- | --- |
| **channel** | A named, configured destination. Never "provider" in prose, never "target" |
| **type** | What a channel is — `telegram` or `email`. The Go interface is `Sender` |
| **send** | What notifio does. The handler is `send`, not `handle` or `notify` |
| **token** | The API secret. Never used for a Telegram bot token, which is a **bot token** |
| **caller** | Whoever POSTs to notifio. Never "client", which is also `http.Client` |
| **recipient** | Where a message goes — a chat id or an address. The field is `to` |

"Notification" is what the product makes and is avoided as a variable name, because everything
here is one.

## Commit messages

**One line. No body, no trailers, ever.**

A full declarative sentence saying what the change accomplishes. Capitalized, no trailing
period, no prefix, no conventional-commit tag, no ticket number, and no `Co-Authored-By`.

```
Refuse a field the channel type has no use for, so a dropped subject cannot go unnoticed
Check the token before reading the body, so a stranger cannot make notifio buffer an upload
Keep a filename's own alphabet, since RFC 2231 is what carries it
```

Two clauses joined by "so" or "and" are common and welcome — the second says why the first was
worth doing. What the message must not be is a label: not `fix: headers`, not `update send`,
not `wip`.

## Comments

Every document under `docs/` opens with a **Contents** list, generated from its own headings,
so a reader can see the shape before committing to the prose. A document short enough not to
need one does not have one.

**Comments are short.** One line, occasionally two. A comment names why a line is the way it is
and stops.

**No prose in comments, and no history.** No paragraphs, no "this used to be", no "changed
from", no narrated alternatives. What a thing used to be is in `git log`; why it is what it is,
at any length worth reading, is in `docs/`. A comment that grew into an essay is a document
living in the wrong file, where nothing updates it and nobody finds it.

**Comments explain why, never what.** A line restating the line under it is noise.

**Package doc comments are one or two lines** saying what the package is for. The argument for
a package's existence goes here in `docs/`, not above its `package` clause.

**Say it once.** The same reason in a function, its test and its caller is three copies to keep
true, and the two that fall behind are the ones somebody will read.

## Go

- `gofmt` clean; CI fails on anything it would rewrite.
- Tests beside sources as `*_test.go`. No `tests/` directory.
- Errors wrap with `%w` and name what was being done.
- One error vocabulary per package: `ErrNotFound`, `ErrConflict`, `ErrInvalid` in
  `internal/tokens`; `ErrRequest` and `ErrLimit` in `internal/send`; `ErrRefused` and
  `ErrUnreachable` in `internal/channel`, which is what lets the handler pick 502 from 504
  without matching on strings.
- `context.Context` on anything that can block, and the caller's context on anything done on
  the caller's behalf.
- A function that returns "this succeeded, and also something happened" returns a bool, not a
  sentinel error. `tokens.Verify` returns `(token, ok, error)` where the error means *the file
  could not be re-read*, not *the token was wrong* — folding those together would turn a
  damaged file into an outage.

### Tests

- **A test that asserts a refusal must prove the refusal happened, not merely that a status
  came back.** The header-injection tests check that nothing was delivered; the max-body test
  checks that the temp files are gone.
- **Test across the seam when two halves have to agree.** `TestAFilenameSurvivesParsingAndEncoding`
  exists because the parser and the MIME encoder each passed their own tests while the parser
  was destroying exactly the filenames the encoder existed to carry.

## Time

- `main.go` pins `time.Local = time.UTC`. Everything logged is UTC.
- Stored as **Unix seconds in an integer field**. Not text, not milliseconds.
