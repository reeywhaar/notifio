# notifio

One HTTP endpoint that turns a POST into a notification. Name a Telegram bot or an SMTP relay
in a config file, mint a token for it, and anything holding that token can send without
learning either protocol.

```
app ──POST /api/send  {body:"Disk at 91%"}──▶ notifio ──▶ api.telegram.org
    ◀────────── {"ok":true,"id":"4821"} ──────────        └──▶ smtp.example.com
      Authorization: Bearer nt_…
                    └─ the token is issued for one channel; that is where this goes
```

## Contents

- [Setup](#setup) — [1. Write config.json](#1-write-configjson) · [2. Run it](#2-run-it) · [3. Mint a token](#3-mint-a-token) · [4. Send](#4-send)
- [Usage](#usage) — the endpoint, the three content types, curl and TypeScript
- [Channels](#channels)
- [Command line](#command-line)
- [Configuration](#configuration)
- [Backups](#backups)
- [How it works](#how-it-works)
- [What it deliberately does not do](#what-it-deliberately-does-not-do)
- [Development](#development)
- Full reference: [docs/](docs/) — [sending](docs/sending.md) · [channels](docs/channels.md) · [tokens](docs/tokens.md) · [nonced](docs/nonced.md) · [deploy](docs/deploy.md)

## Setup

### 1. Write config.json

**notifio will not start without it**, so it comes first — finding that out from a crash loop
is a worse first minute than reading one paragraph.

```json
{
  "version": 1,
  "channels": {
    "alerts": {
      "type": "telegram",
      "token": "123456789:AAE-your-bot-token",
      "pinned": { "to": "-1001234567890" }
    },
    "notices": {
      "type": "email",
      "host": "smtp.example.com",
      "user": "notifio@example.com",
      "password": "the-relay-password",
      "pinned": { "from": "Notifio <notifio@example.com>" }
    }
  }
}
```

Everything outside `pinned` is how to reach the provider. Everything inside it is part of the
message, and **a pinned field cannot be sent by a caller** — `alerts` above always posts to that
chat, and `notices` always sends as that address. Every key is in
[docs/channels.md](docs/channels.md#every-key), because JSON cannot carry comments;
`config.example.json` has them all filled in.

This file goes in the volume, at `/data/config.json`. It holds your credentials in cleartext:
keep it out of a repository, and read [Backups](#backups) before turning those on.

### 2. Run it

**notifio listens on `:80` inside the container**, always. Behind a reverse proxy that means
nothing needs publishing to the host; on its own, remap it with `-p`.

With compose, behind [caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy):

```yaml
services:
  notifio:
    image: ghcr.io/reeywhaar/notifio:latest
    restart: unless-stopped

    volumes:
      - data:/data              # required: notifio will not start without it

    networks:
      - caddy

    labels:
      caddy: notify.example.com
      caddy.reverse_proxy: "{{upstreams 80}}"

volumes:
  data:

networks:
  caddy:
    external: true
```

[`docker-compose.yml`](docker-compose.yml) is that file with every setting present and
commented at its default, plus the backup sidecar.

Or directly, publishing a port for local use:

```sh
docker run -d --name notifio -v notifio-data:/data -p 8080:80 ghcr.io/reeywhaar/notifio:latest
```

The `-v` is the one flag you cannot omit.

### 3. Mint a token

```sh
TOKEN=$(docker exec notifio notifio token add grafana alerts)
```

**A token is minted for one channel and can send through no other.** A service that needs two
gets two tokens — that is the intended shape, and it means a leaked one exposes one
destination. See [docs/tokens.md](docs/tokens.md#the-token-is-the-channel).

**Shown once.** It is stored hashed, so a lost token is removed and minted again rather than
recovered. The doubled `notifio notifio` is not a typo: `docker exec` bypasses the image
`ENTRYPOINT`, so the binary has to be named.

The running server picks it up on the next request — there is nothing to restart.

**Or mint one whose secret never crosses the wire:**

```sh
SECRET=$(docker exec notifio notifio token add grafana alerts --nonced)
```

The caller then sends a hash of it with a timestamp, good for five minutes, instead of the
secret itself — so a value captured from a log or a proxy is already useless. **It is still one
value to configure**: the token id is `sha256(secret)[:8]`, derived by the caller rather than
handed to it.

```sh
ID=$(printf %s "$SECRET" | sha256sum | cut -c1-8)
TS=$(date +%s)
WIRE="ntc_$TS.$ID.$(printf '%s.%s.%s' "$TS" "$ID" "$SECRET" | sha256sum | cut -d' ' -f1)"
curl -X POST … -H "Authorization: Bearer $WIRE" --data-urlencode 'body=…'
```

[docs/nonced.md](docs/nonced.md#making-one) has that as a reusable bash function and as a
TypeScript one, both of which take either kind of secret and branch on its prefix.

It costs something real — notifio has to keep that secret in `data.json` in the clear, where a
bearer token is only ever a hash. [docs/nonced.md](docs/nonced.md) has the trade in full.

### 4. Send

```sh
curl -X POST http://localhost:8080/api/send \
  -H "Authorization: Bearer $TOKEN" \
  --data-urlencode 'body=Disk at 91%'
```

That is the whole product.

> Use `--data-urlencode`, not `-d`. `-d` sends its argument raw, so a `%` or a `&` in your
> message is either an invalid escape or a second field, and notifio answers `400`.

## Usage

**The request says what to send, never where.** The token decides the channel, the channel
decides the type, and the type decides which fields are valid — all before the body is read.

There is **no `channel` field**, and sending one is a `400` rather than being ignored.

### On a telegram token

| field | required | notes |
| --- | --- | --- |
| `to` | only if the channel does not pin it | Exactly one chat id or `@channelusername` |
| `body` | yes | Up to 4096 characters |
| `body_type` | no | `plain` (default), `md`, `html` |
| `attachments` | no | Up to 10, each sent as a document |
| `link_preview` | no | `false` stops Telegram expanding a link into a preview card below the message. Default `true` |

### On an email token

| field | required | notes |
| --- | --- | --- |
| `to` | only if the channel does not pin it | Repeatable for several recipients. Do **not** comma-separate |
| `from` | only if the channel does not pin it | |
| `subject` | yes | |
| `body` | yes | |
| `body_type` | no | `plain` (default), `html`. `md` is refused |
| `attachments` | no | |

**`md` is CommonMark and means the same thing on every channel.** notifio renders it — to
Telegram's inline-only HTML subset, or to ordinary HTML for mail — so one body works against
both and there is nothing to escape by hand.

There are two kinds of mismatch, and only one is an error. A field that would **silently do
nothing** is refused: `channel` was already decided by the token, and `from` has nowhere to go
on a Telegram message. A field the type simply **cannot use** is absorbed — a `subject` becomes
a bold title on Telegram, a missing one becomes `No subject` on email, and `link_preview` is
ignored there. A generic sender cannot know which kind of channel its token points at, and
should not lose a notification over that.

**What a channel pins, a caller cannot change.** One rule, both types:

> A request may state what is pinned. It may not contradict it. A field that is not pinned must
> be sent.

So a channel that pins `to` is a fixed destination — its token reaches that chat or address and
no other — and one that does not lets every request choose. Which of the two a token has is
visible in the config.

Restating a pinned value is accepted, so a caller that keeps its own copy of the destination
keeps working. Sending a *different* one is a `400` rather than a silent redirect.

### The three content types

JSON, `multipart/form-data`, and `application/x-www-form-urlencoded` all work and produce the
same request. Use multipart for attachments of any size: JSON carries them as base64 and has to
decode them in memory.

```sh
# JSON
curl -X POST https://notify.example.com/api/send \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"to":["ops@example.com"],"subject":"Deploy finished","body":"<b>v2.1</b> is live","body_type":"html"}'

# Multipart, with a file
curl -X POST https://notify.example.com/api/send \
  -H "Authorization: Bearer $TOKEN" \
  -F 'to=ops@example.com' -F 'subject=Nightly report' -F 'body=attached' \
  -F 'attachments=@report.csv'
```

### TypeScript

```ts
const form = new FormData()
form.set("body", "Deploy finished")
form.append("attachments", new File([log], "deploy.log", { type: "text/plain" }))

const res = await fetch("https://notify.example.com/api/send", {
  method: "POST",
  headers: { authorization: `Bearer ${process.env.NOTIFIO_TOKEN}` },
  body: form,          // no content-type header: fetch sets the boundary
})
if (!res.ok) throw new Error((await res.json()).error)
```

Do not set `content-type` by hand with `FormData` — that omits the boundary, and the request
fails with a `400` that looks like the server's fault.

### Responses

```json
{"ok":true,"channel":"alerts","type":"telegram","id":"4821","dur_ms":312}
{"ok":false,"code":"provider","error":"Bad Request: chat not found"}
```

`400` means you sent it wrong. `422` means it is too big for where it is going. `502` means the
provider refused it, with the provider's own words. The full table is in
[docs/sending.md](docs/sending.md#responses).

## Channels

Two types. Both are defined in `config.json` and neither needs a restart to add.

**telegram** — a bot token, and optionally a pinned chat. Attachments are sent as documents, so
an image arrives as a file rather than an inline preview. A `subject` becomes a bold title,
since Telegram has none of its own.

**email** — an SMTP relay, with `to` and `from` each pinnable. STARTTLS by default, and notifio
refuses to send if a relay that was configured for STARTTLS does not offer it. `Date`, `Message-ID` and `MIME-Version` are
generated for you; non-ASCII subjects and filenames are encoded properly; any rejected
recipient fails the whole send, so a `200` never means "some of them".

Details, limits and the reasoning are in [docs/channels.md](docs/channels.md).

## Command line

```
notifio serve                            the server. the image's CMD
notifio version

notifio token add <label> <channel>      mint one. prints the secret, once
          [--nonced]                     ...one whose secret never crosses the wire
notifio token list [--json]              label, channel, type, created
notifio token remove <label>

notifio channel list [--json]            name, type, destination. never a credential
notifio channel test <name> [--to ADDR] [--body-type html|plain|md]

notifio backup now
notifio healthcheck                      the image's HEALTHCHECK
```

**`notifio channel test` is how you find out a relay is unreachable**, rather than by a caller's
first `502`. It sends a real message through the real provider and prints what came back.

The message is **marked up by default**, because a plain one proves the connection and says
nothing about whether `parse_mode` or an HTML part survives — which is the half that actually
breaks. `--body-type plain` or `md` sends the others.

stdout carries the answer and stderr everything else, so `TOKEN=$(… token add x alerts)`
captures exactly the token.

## Configuration

`config.json` holds channels and nothing else. Two environment variables, both optional:

| variable | default | meaning |
| --- | --- | --- |
| `NOTIFIO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `NOTIFIO_BACKUP_URL` | unset | A backup sidecar's `POST /backup`. Unset means no backups |

Per-channel settings — `max_body`, `send_timeout`, `api_base` — live on the channel, because
the limits they describe are the provider's and the provider differs. See
[docs/deploy.md](docs/deploy.md#environment).

## Backups

**The archive holds every credential notifio has, in cleartext**, because notifio has to present
them to the provider and cannot hash them. Set `BACKUP_PASSWORD` on the sidecar, which repacks
each archive as an AES-256 zip before upload. An unencrypted backup of this file is worse than
no backup.

Set `NOTIFIO_BACKUP_URL` and notifio posts a `.tgz` of `config.json` and `data.json` to a
[backio-agent](https://github.com/reeywhaar/backio/tree/main/agent) sidecar whenever either has
actually changed, checked every five minutes.

notifio sends the archive and a name and nothing else — the token, the remote and the path live
in the sidecar, so a compromised notifio cannot reach an existing backup. Restoring is `tar xzf`
into the volume; see [docs/deploy.md](docs/deploy.md#backups).

## How it works

One binary. A request arrives, the bearer token is looked up in `data.json`, and that token
names a channel in `config.json`. The channel's type picks a validator and a sender; a `md`
body is rendered for that type; the sender talks to Telegram over HTTPS or to a relay over
SMTP; and the result is the response.

Both files are watched by mtime, so minting a token or adding a channel needs no restart. If
`config.json` stops parsing, notifio keeps serving the channels it already had, logs it once,
and reports `"config":"stale"` from `/healthz` — a typo should not take down a running
notifier.

Nothing is queued, stored or retried — a send's result is its HTTP status, and the access log is
the only record.

## What it deliberately does not do

- **Queue or retry.** A send is synchronous and its result is the response. Retrying belongs to
  the caller, who knows whether a duplicate alert is worse than a missing one.
- **Anything inbound.** No webhook, no `getUpdates`, no IMAP, no bounce handling. It sends.
- **Several channels in one request.** A fan-out that half succeeds has no honest status code.
  Two tokens, two requests, and you know which one failed.
- **Templates.** The caller composes the text.
- **MarkdownV2.** `md` is CommonMark, rendered per channel, so one body works everywhere.
- **Typed Telegram media.** Attachments work; every one is a document. Picking `sendPhoto` by
  content type brings Telegram's album type-mixing rules with it.
- **`multipart/alternative` email**, or any HTML-to-text fallback.
- **`cc` and `bcc`**, rate limiting, quotas, metrics, or scheduling.
- **Config interpolation.** No `${VAR}`; values are the bytes in the file.

Each of these is a decision with a reason, recorded in [docs/](docs/) so it is not re-argued
every few months.

## Development

```sh
go test ./...

mkdir -p /tmp/notifio && cp config.example.json /tmp/notifio/config.json
NOTIFIO_DATA_DIR=/tmp/notifio NOTIFIO_CONFIG=/tmp/notifio/config.json PORT=8080 go run . serve
```

The end-to-end assertions run against a built image:

```sh
docker build -t notifio:test .
IMAGE=notifio:test .github/smoke.sh
```

Conventions — commits, comments, naming, Go rules — are in
[docs/conventions.md](docs/conventions.md).

## Notifying from GitHub Actions

`ghactions/notify` is a composite action in this repository, and notifio's own CI uses it to
report its builds. Two secrets, and nothing that says where the message lands:

```yaml
- uses: reeywhaar/notifio/ghactions/notify@main
  with:
    host: ${{ secrets.NOTIFIO_HOST }}
    token: ${{ secrets.NOTIFIO_TOKEN }}
    body: "🔔 **${{ github.repository }}** deployed"
```

It prints nothing on success and the failure in full, so a green step stays quiet. The
provider's message id is available as `outputs.id` rather than printed.

Hand it a [nonced secret](docs/nonced.md) and it signs rather than sending — same workflow, the
secret never leaves the runner.

Note that **the body appears in the log either way**: GitHub echoes a step's `with:` inputs
before running it. Secrets are masked as `***`, so anything that must not be in a public build
log belongs in one.

The token names the channel and the channel pins its destination, so moving those
notifications is a config edit on the notifio instance rather than a change in every repository
that sends them.

Pass `subject:` when the token points at an email channel, which requires one, and leave it out
for a Telegram channel, which has no such field and refuses it. notifio's own CI notifies
through a Telegram channel with a pinned `to`, so it sends a body and nothing else.
