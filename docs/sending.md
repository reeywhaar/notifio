# Sending

The contract `internal/send` implements.

## The endpoint

```
POST /api/send
Authorization: Bearer nt_…
Content-Type: application/json | multipart/form-data | application/x-www-form-urlencoded
```

Exact path, `POST` only. Any other method is `405` with `Allow: POST`; any other path is `404`.
The only other route is `GET /healthz`.

## Authentication

`Authorization: Bearer <token>`. Missing, malformed, or unknown is `401` — the same body and
status for all three, because the distinction tells a guesser which half was right.

**Auth is checked before the body is touched**, which is why the token is not a form field: a
token in the body cannot be checked until the body is parsed, and that would let an
unauthenticated stranger make notifio buffer a 25 MB upload, to disk, on every request.

**Auth also decides the schema.** The token resolves to a channel and a type, so by the time
the body is parsed notifio already knows whether this is a telegram or an email request and
which validator to run. That is why the type is not in the body: it was never the caller's to
choose. See [tokens.md](tokens.md).

`/healthz` is the one unauthenticated path.

## The three content types

All three produce the same request. `Content-Type` decides the parser and nothing else;
anything else is `415`.

| content type | how fields arrive | attachments |
| --- | --- | --- |
| `application/json` | object keys | `attachments: [{filename, content_type, data}]`, `data` is base64 |
| `multipart/form-data` | form fields | repeated `attachments` file parts |
| `application/x-www-form-urlencoded` | form fields | **none possible**; an `attachments` field here is `400` |

Repeated scalar fields are `400` rather than resolved. `body` given twice has a first-wins and
a last-wins answer, and choosing either sends a message somebody did not write. Only `to` and
`attachments` may repeat.

An unknown JSON field is `400`, so a misspelled `body_typ` is a refusal rather than a default.

## One field is common; the rest belongs to a type

| field | cardinality | value |
| --- | --- | --- |
| `body` | exactly 1 | The message. Non-empty after trimming whitespace |

Three rules apply to both types:

- **A present-but-empty value is `400`, never treated as absent.** `to=` is not "fall back to
  the channel default" — it is a template that failed to interpolate, and falling back sends
  somebody's message to the wrong place.
- **A field is refused where its type has no use for it.** Not ignored — see
  [below](#refused-where-it-has-no-meaning).
- **A request may state what the channel pins, and may not contradict it.** A channel's
  `pinned` block fixes `to`, and on email `from` as well. Sending the same value is accepted;
  sending a different one is `400`. A field that is not pinned must be sent. See
  [channels.md](channels.md#pinned).

### A request on a `telegram` token

| field | cardinality | required | value |
| --- | --- | --- | --- |
| `to` | **exactly 1** | only when the channel does not pin it | A numeric chat id, or `@channelusername`. **Must match the pin if the channel has one**; two values are `400`, not "the first one" |
| `body` | 1 | yes | Up to 4096 characters. Over that is `422`, never truncated |
| `body_type` | 0–1 | no, default `plain` | `plain`, `md`, `html` |
| `attachments` | 0–10 | no | Each sent as a document. 11 or more is `422` |
| `link_preview` | 0–1 | no, default `true` | Whether Telegram expands a link in the body into a preview card below the message. `true`/`false`/`1`/`0`/`yes`/`no`/`on`/`off`, and **anything else is `400`** |
| `subject` | — | **refused** | |
| `from` | — | **refused** | |

```json
{"to":"-1001234567890","body":"Disk at 91%","body_type":"md"}
```

### A request on an `email` token

| field | cardinality | required | value |
| --- | --- | --- | --- |
| `to` | **1..n** | only when the channel does not pin it | Repeatable. Each value is one RFC 5322 address; a value holding two is `400`. Duplicates collapse, order kept |
| `from` | 0–1 | only when the channel does not pin it | One address, optionally with a display name |
| `subject` | **exactly 1** | yes | Non-empty after trimming. A line break is `400` |
| `body` | 1 | yes | Bounded only by the channel's `max_body` |
| `body_type` | 0–1 | no, default `plain` | `plain`, `html`. **`md` is refused** |
| `attachments` | 0..n | no | Bounded by `max_body`, not by count |

```json
{"to":["ops@example.com"],"subject":"Disk at 91%","body":"<b>91%</b>","body_type":"html"}
```

A comma-separated `to` is `400` rather than split, because `"Doe, Jane" <jane@example.com>` is
a single valid address containing a comma — splitting on commas is wrong for exactly the
recipients whose names are written properly. Repeat `to` instead.

### Refused where it has no meaning

| field | on a `telegram` token | on an `email` token |
| --- | --- | --- |
| `channel` | **400** — the token decides | **400** |
| `to` | **400** if it differs from the pin | **400** if it differs from the pin, else required |
| `from` | **400** | **400** if it differs from the pin, else required |
| `link_preview` | **400** if it differs from the pin | **ignored** — email has no preview cards |
| a second `to` | **400** | accepted |

`channel` is the entry most likely to be hit — somebody copying an example from another service,
or from an older draft of this one. It is refused rather than ignored because the token already
decided the channel, and a field that does nothing teaches a caller that it does something.

**There are two kinds of mismatch here, and only one of them is an error.**

A field that would *silently do nothing* is refused: `channel` was already decided by the token,
and `from` has nowhere to go on a Telegram message. Accepting either would teach a caller that
it works.

A field the type simply *cannot use* is absorbed instead. A `subject` becomes a bold title on
Telegram, a missing one becomes `No subject` on email, and `link_preview` is ignored on email.
A notification that arrives looking slightly odd beats one that did not arrive, and a generic
sender — a CI job, say — cannot know which kind of channel a token points at.

## Body types

| `body_type` | Telegram | email |
| --- | --- | --- |
| `plain` (default) | sent as-is | `text/plain; charset=utf-8`, quoted-printable |
| `md` | **CommonMark**, rendered to Telegram's HTML subset | **CommonMark**, rendered to HTML |
| `html` | sent as-is with `parse_mode: HTML` | `text/html; charset=utf-8`, quoted-printable |

**`md` is CommonMark and means the same thing on every channel.** notifio parses it once and
renders it for whichever provider the token points at, so one body works against both:

```
# Deploy finished       telegram  <b>Deploy finished</b>
**v2.1.0** is live        ──▶     <b>v2.1.0</b> is live
- 12 commits                      • 12 commits

                        email     <h1>Deploy finished</h1>
                          ──▶     <p><strong>v2.1.0</strong> is live</p>
                                  <ul><li>12 commits</li></ul>
```

Telegram has no block elements — no `<p>`, no `<br>`, no lists, no headings — so structure
becomes whitespace there: a heading is bold, a bullet is `•`, a rule is `———`, and an image
becomes a link, because Telegram will not inline one from message text. Details in
[channels.md](channels.md#markdown).

**Raw HTML inside `md` is dropped** — the tags, not the text between them. A caller who wants
HTML has `body_type: html`; letting tags through here would mean Telegram rejecting a whole
message over one it does not know.

## Attachments

- `filename` is required. In JSON it is a field; in multipart it comes from the part headers.
- **The filename is reduced to a base name**, with path separators, the characters a Windows
  path reserves, and control characters replaced. Its alphabet is left alone: `отчёт.csv` is a
  good name and both the MIME header and the Telegram upload carry it.
- `content_type` defaults to `application/octet-stream`.
- **The whole request body is capped by the channel's `max_body`** (default 25 MiB), enforced
  before parsing. Over it is `413`.
- **Multipart spills to disk over 1 MiB** and is streamed from there, so a large send costs
  disk rather than heap. JSON cannot do this — base64 in a JSON document is decoded in memory —
  so **prefer multipart for anything large**.

## Responses

```json
{"ok":true,"channel":"alerts","type":"telegram","id":"4821","dur_ms":312}
```

`id` is the provider's own: a Telegram `message_id`, or the `Message-ID` notifio generated for
an email. Failure is the same shape, always JSON:

```json
{"ok":false,"code":"provider","error":"Bad Request: chat not found"}
```

| status | `code` | when |
| --- | --- | --- |
| 400 | `request` | Bad JSON, a field the type refuses, a missing required one, a repeated scalar, an attachment with no filename |
| 401 | `auth` | Token missing or unknown |
| 404 | `request` | Any path but `/api/send` and `/healthz` |
| 405 | `request` | `/api/send` with another method |
| 413 | `request` | Body over the channel's `max_body` |
| 415 | `request` | A content type notifio does not parse |
| 422 | `limit` | Well-formed, and the provider will not take it: a body over Telegram's 4096, more than 10 attachments |
| 500 | `config` | The token file could not be read |
| 503 | `config` | The token is valid and the channel it was issued for is no longer configured |
| 502 | `provider` | The provider refused it, or could not be reached. `error` is the provider's own words |
| 504 | `provider` | The provider did not answer within the channel's `send_timeout` |

**422 is separate from 400 so a caller can tell "I sent this wrong" from "this is too big for
where it is going".** The first is a code change; the second is usually an automated sender
that needs to truncate its own output.

**A provider's rejection is 502, not 400.** notifio's caller did nothing wrong — a chat id that
is no longer valid is the operator's problem — and mapping it to 400 would tell an alerting
script to stop retrying when the right answer is to wake somebody.

### Partial success

- **Email: any rejected recipient fails the whole send**, `502`, and nothing is delivered to
  anyone. A caller that got a `200` must never have to wonder who received it.
- **Telegram: a media group is one call.** The exception is a body too long to be a caption,
  which goes as its own message before the files; if the files then fail, the response carries
  `"partial": true`. That field exists for the one case that genuinely cannot be atomic.

## `/healthz`

`GET /healthz` → `200 {"ok":true,"version":"…","channels":3,"tokens":2}`, unauthenticated. A
`"config":"stale"` alongside those means `config.json` will not parse and the channels being
reported are the last ones that did — see [channels.md](channels.md#the-file).

It reports that the process is answering and how many channels it is serving. It deliberately does
**not** reach out to Telegram or the relay: a healthcheck that fails because somebody else's
server is down restarts a container that was working fine. `notifio channel test` is the
command for "is the relay actually reachable", and a person runs it.
