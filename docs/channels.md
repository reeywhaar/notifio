# Channels

A channel is a named, configured destination. This document is every key it can have, and then
what each type does with them.

## Contents

- [The file](#the-file)
  - [Every key](#every-key)
  - [`pinned`](#pinned)
  - [Values are literal](#values-are-literal)
- [telegram](#telegram)
  - [Markdown](#markdown)
  - [A subject becomes a title](#a-subject-becomes-a-title)
  - [Errors](#errors)
  - [Link previews](#link-previews)
- [email](#email)
  - [The message](#the-message)

## The file

`/data/config.json`, beside `data.json` in the same volume. Override the path with
`NOTIFIO_CONFIG`; the image sets it to the default.

```json
{
  "version": 1,
  "channels": {
    "alerts": {
      "type": "telegram",
      "token": "123456789:AAE-…",
      "pinned": { "to": "-1001234567890" }
    },
    "notices": {
      "type": "email",
      "host": "smtp.example.com",
      "user": "notifio@example.com",
      "password": "…",
      "pinned": { "from": "Notifio <notifio@example.com>" }
    }
  }
}
```

`config.example.json` ships beside it with every key filled in.

**A missing or invalid config file is a refusal to start**, unlike a missing `data.json`, which
just means no tokens yet. A notifio with no tokens is a working program waiting for its first
caller; a notifio with no channels cannot do anything at all, and starting it would only delay
the error until somebody's alert fires.

**The file is reloaded by mtime**, so adding a channel needs no restart. The server `stat`s it
on each request and re-reads when it has moved; there is no polling loop, so an idle notifio
touches nothing.

A reload that fails validation **keeps the previous config in memory** and carries on sending
through the channels already loaded — a typo must not take down a running notifier. It is
reported in two places:

- **`ERROR` in the log**, once when it breaks and once more when it loads again. Not on every
  request: the container healthcheck asks every thirty seconds, and the same line repeated
  until somebody notices is how a log stops being read.
- **`"config": "stale"` in `/healthz`**, which stays `200`. A `503` would fail the container
  healthcheck, and the restart that followed would find the same broken file and refuse to
  start at all — turning a notifier that is still sending into one that is not.

One thing the reload does not revisit: **`serve` sizes the HTTP write timeout from the longest
`send_timeout` at startup**, so raising one on a live instance needs a restart to take full
effect. Every other channel change is picked up as it is written.

### Every key

Shared by both types:

| key | required | default | meaning |
| --- | --- | --- | --- |
| `type` | yes | — | `telegram` or `email` |
| `max_body` | no | `25MiB` | Request body cap, attachments included. `"50MiB"`, `"10MB"`, or a number of bytes |
| `send_timeout` | no | `60s` | The whole provider conversation, upload included |
| `pinned` | no | — | Request fields this channel fixes — see [below](#pinned) |

`telegram` only:

| key | required | default | meaning |
| --- | --- | --- | --- |
| `token` | yes | — | The bot token, shaped `<digits>:<rest>`. Not the `bot` prefix from the URL |
| `api_base` | no | `https://api.telegram.org` | Where the Bot API is |

`email` only:

| key | required | default | meaning |
| --- | --- | --- | --- |
| `host` | yes | — | The relay |
| `port` | no | `587` | — |
| `encryption` | no | `starttls` | `starttls`, `tls` (implicit, usually 465), or `none` |
| `user`, `password` | no | — | SMTP AUTH PLAIN. Refused with `encryption: none` |

### `pinned`

| key | telegram | email |
| --- | --- | --- |
| `to` | one chat id or `@channelusername` | one address, or an array of them |
| `from` | — | one address |
| `link_preview` | `true` or `false` | — (email has no preview cards) |

**Everything outside `pinned` is how to reach the provider; everything inside it is part of the
message.** That is why they are separate blocks, and it is what makes the rule mechanical
rather than something to memorise:

> **A request may state what is pinned. It may not contradict it. A field that is not pinned
> must be sent.**

Sending a pinned field is fine when it names exactly what is pinned — saying the same thing is
not a disagreement, and refusing it would break a caller that documents its own destination in
its own config. Sending a *different* value is `400`, because a caller told their message went
to one place while it went to another has been lied to with a `200`.

The comparison is textual, after trimming, and order does not matter for a list of recipients.
`"Notifio <n@x.com>"` and `"n@x.com"` name the same mailbox and are **not** the same value — a
rule that sometimes looked through a display name would be one nobody could predict. State what
is pinned, or state nothing.

```json
"ops":    { "type": "email", "host": "…", "pinned": { "from": "n@x.com", "to": "ops@x.com" } },
"notify": { "type": "email", "host": "…", "pinned": { "from": "n@x.com" } }
```

`ops` takes a subject and a body and nothing else; its token can write to `ops@x.com` and
nowhere else. `notify` takes a `to` as well, and its token can write anywhere — same relay,
same credential, different authority. Which of the two a token has is visible in the config
rather than inferred from how anybody happens to call it.

A bot that usually posts to one chat and occasionally elsewhere is therefore two channels and
two tokens, which is also two things to revoke.

**Pinned addresses are validated at startup**, because a caller cannot correct one.

**`subject` and `body` are not pinnable, and that is the line.** `pinned` holds where a message
goes and who it is from — properties of the channel. What the message *says* is the caller's,
every time, and a config that fixed it would be a template engine.

A channel that exists to carry notifications wants
`"pinned": { "to": "…", "link_preview": false }` on Telegram, or
`"pinned": { "to": "…", "from": "…" }` on email — the destination settled once, the message
still the sender's.

**An unknown key is a startup error**, including a key belonging to the other type. A
misspelled `passwrod` that loaded cleanly would be a channel with no password and a confusing
failure at send time.

`max_body` and `send_timeout` are per-channel because the limits they describe are somebody
else's, and the somebody differs: Telegram takes a 50 MB document from a bot, and a hosted
relay often stops at 25 MB or 10. One global cap has to be the smallest of them, silently
capping the channel that could take more — or the largest, turning a refusal notifio could make
instantly into a full upload followed by the provider's rejection.

`api_base` is how notifio runs where `api.telegram.org` is blocked: point one channel at a
relay that is reachable and leave another on the default.

A telegram channel pins `to` and nothing else — a Telegram message has no sender to choose.

### Values are literal

**Nothing in `config.json` is interpolated.** No `${VAR}`, no `$(…)`, no `@file` indirection. A
password containing a `$` is a password containing a `$`.

That gives up `"password": "${SMTP_PASSWORD}"`, which is a real want and is deliberately not
solved by string substitution — the failure mode of an undefined variable is a channel
authenticating with an empty password, and every implementation then grows an escape for a
literal `$`, a decision about `${A:-b}`, and a question about whether expansion happens before
or after validation. If config-from-environment is wanted it gets designed then, as its own
thing.

Until then the file holds the secret, which is why it is in a volume rather than a repository,
and why [deploy.md](deploy.md#backups) insists on the backup sidecar's encryption.

## telegram

Requests go to `<api_base>/bot<token>/<method>`. Which method depends on how many attachments
there are:

| attachments | call |
| --- | --- |
| 0 | `sendMessage` |
| 1 | `sendDocument`, the body as `caption` |
| 2–10 | `sendMediaGroup`, the body as the first item's caption |
| 11+ | `422` |

**Every attachment is sent as a `document`,** never as a `photo`, `video` or `audio`. Telegram
restricts which media types may share an album — photos and videos may mix, documents may not
join them — and choosing per-file by content type produces a request that works until somebody
attaches a PNG and a CSV together. The cost is that **an image arrives as a file rather than an
inline preview**, which is the first thing anybody notices.

**A body over 1024 characters cannot be a caption**, so with attachments present it is sent as
its own `sendMessage` first and the files follow with none. Two calls, text first, so it
arrives above its attachments. Truncating to 1024 would throw away the part of an alert most
likely to matter. This is the only path that can half fail, and it is why a response can carry
`"partial": true`.

A body over 4096 with no attachments is `422`, not split: every boundary is inside somebody's
code block or between a `*` and its partner.

A `sendMediaGroup` names its files with `attach://file0` entries in its `media` array, and the
multipart parts are named to match. That is the one corner of the Bot API that is not obvious
from the method signature.

### Markdown

`md` is **CommonMark, and notifio renders it** into the subset of HTML the Bot API accepts. It
means the same thing on an email channel, which is the point: a caller writes one body and it
works wherever the token happens to send.

Telegram's markup is inline-only, so structure has to become whitespace:

| CommonMark | Telegram |
| --- | --- |
| heading | `<b>…</b>` and a blank line |
| paragraph | text and a blank line |
| `- item` | `• item` on its own line |
| `1. item` | `1. item` on its own line |
| `> quote` | `<blockquote>` |
| fenced code | `<pre>` |
| `---` | `———` |
| image | a link to it — Telegram will not inline one from message text |
| raw HTML | **dropped**, tags only; the text between them stays |

**MarkdownV2 is not offered, and that is the decision worth defending.** Passing it through
would have made `md` mean one thing on Telegram and something else on email, and it puts
eighteen characters — ``_ * [ ] ( ) ~ ` > # + - = | { } . !`` — on the caller to escape, where
forgetting one comes back as a `400`. Rendering costs one dependency and removes that class of
bug entirely.

`html` is passed through untouched, which on Telegram means its short list of inline tags and
nothing else.

### A subject becomes a title

Telegram has no subject. A request that carries one gets it as a bold first line above the body
rather than a `400`, because a generic sender cannot know which kind of channel its token points
at, and a notification that arrives looking slightly odd beats one that did not arrive.

The title is escaped, and it is the only markup notifio authors rather than relays. It is applied
after any markdown rendering, so it only ever has to speak plain text or HTML. A missing subject
on an **email** channel becomes `No subject` for the same reason.

A line break in an email subject is still a `400`: that is header injection, which is a different
thing from a caller who had nothing to put in the field.

### Errors

Telegram answers `{"ok":false,"error_code":400,"description":"…"}` and the description is
relayed verbatim as a `502` — it is the only thing that says what to fix.

### Link previews

Telegram expands a link in a message into a **preview card** below it — the page's title, a
description and an image. For a build notification, which is mostly links, that is a screenful
of noise per message, so a channel like that wants `"pinned": { "link_preview": false }`.

`link_preview: false` sends Telegram's `link_preview_options` rather than the deprecated
`disable_web_page_preview`, and applies to `sendMessage` only — a caption on a document does not
generate a preview in the first place.

**Email has no equivalent and ignores the field.** A mail client shows a link as the message
wrote it and never fetches the page to expand it, so there is nothing for the setting to turn
off — and a sender that cannot know which kind of channel it is talking to should not lose a
message over a field that is merely irrelevant.

A `429` carries `parameters.retry_after`. **notifio does not sleep and retry**; it answers `502`
with the hint in the message, because holding a caller's HTTP request open through Telegram's
backoff is a way to run out of sockets during an incident, which is exactly when every alert
fires at once.

## email

A plain SMTP submission. By `encryption`:

| value | how | auth |
| --- | --- | --- |
| `starttls` (default, 587) | dial, require the server to advertise `STARTTLS`, upgrade | PLAIN after the upgrade |
| `tls` (465) | TLS from the first byte | PLAIN |
| `none` (25) | plaintext | **refused at config load** |

**The "must advertise STARTTLS" check is load-bearing.** Without it, a relay that has lost its
certificate downgrades to plaintext, the credentials go out in the clear, and nothing anywhere
reports it. A relay that does not offer STARTTLS when STARTTLS was configured is an error.

### The message

```
From: Notifio <notifio@example.com>
To: ops@example.com, oncall@example.com
Subject: =?utf-8?q?Disk_at_91=25?=
Date: Sat, 12 Sep 2026 04:00:00 +0000
Message-ID: <9f86d081884c7d65@example.com>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="…"
```

Five things here are not obvious:

- **`Date`, `Message-ID` and `MIME-Version` are generated by notifio.** A message without them
  is accepted by a permissive relay and scored as spam by everything downstream. The
  `Message-ID` domain comes from the `From` address.
- **The subject is RFC 2047 encoded**, which leaves ASCII alone and encodes anything else.
  Separately and first, a `subject`, `from` or `to` containing CR or LF is refused with `400` —
  header injection is the one input that turns a notifier into an open relay, and it is refused
  explicitly rather than left to the encoder to swallow.
- **Attachment filenames are RFC 2231 encoded**, which is what makes a non-ASCII name survive.
- **Base64 content is wrapped at 76 columns.** RFC 5322 caps a line at 998 octets; Gmail
  accepts one long line and strict relays do not.
- **Text parts are quoted-printable**, because an unencoded UTF-8 body can exceed 998 bytes on
  one line and `8BITMIME` is not guaranteed.

Structure: no attachments → a single `text/plain` or `text/html` part. With attachments →
`multipart/mixed`, the body first.

**There is no `multipart/alternative` and no auto-generated plain-text twin for an HTML body.**
Producing one means an HTML-to-text renderer, which is a dependency for a fallback almost
nothing reads. This is a known gap, not an oversight — and a narrower one now that `md` is
rendered, since the source of a `md` body would make a good plain-text part.

**Any rejected recipient fails the whole send.** A caller that got a `200` must never have to
wonder who received it.

The envelope sender is the address parsed out of `from`; the `From` header keeps the display
name. **Relays commonly rewrite one or both**, which is worth knowing before it is reported as
a notifio bug.
