# Deploy

## Contents

- [The image](#the-image)
- [Port](#port)
- [The volume](#the-volume)
- [Environment](#environment)
- [Behind caddy-docker-proxy](#behind-caddy-docker-proxy)
- [Health](#health)
- [Backups](#backups)
  - [Restore](#restore)
- [What the logs contain](#what-the-logs-contain)
  - [`client`, behind a proxy](#client-behind-a-proxy)
- [CI](#ci)
  - [The notify action](#the-notify-action)
  - [What a CI log still shows](#what-a-ci-log-still-shows)
- [Development](#development)

## The image

`ghcr.io/reeywhaar/notifio:latest`, `linux/amd64` and `linux/arm64`. Alpine plus a static
binary and `ca-certificates`.

`ca-certificates` is load-bearing in both directions: without it every Telegram call fails
certificate verification and every STARTTLS upgrade fails too.

## Port

`:80`, inside the container. Remap it with `-p`. A port number inside a container is not a
thing an operator should have to think about twice.

`PORT` exists for running two copies on a laptop without a container. The image never sets it,
and it is not part of the deployment surface — see [Development](#development).

## The volume

`/data`, holding `config.json` (yours) and `data.json` (notifio's).

**notifio refuses to start without it**, and the two failures say different things:

```
notifio: /data does not exist: mount a volume at /data
notifio: /data/config.json does not exist: write it before starting — see docs/channels.md
```

The directory is also probed for writability at startup, so a read-only mount fails then rather
than at the first `token add` six weeks later.

**The Dockerfile deliberately declares no `VOLUME /data` and creates no directory.** With
`VOLUME` declared, Docker silently creates an anonymous volume, `/data` always exists, the check
could never fire, and the credentials would live somewhere that disappears with the container —
the exact failure the check exists to prevent.

## Environment

Two variables, and they are the only ones the image documents. Both are optional.

| variable | default | meaning |
| --- | --- | --- |
| `NOTIFIO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `NOTIFIO_BACKUP_URL` | unset | A backio-agent sidecar's `POST /backup`. Unset means no backups |

Everything else about a destination lives on its channel in `config.json` — see
[channels.md](channels.md#every-key) — because it is a property of that destination and not of
the process.

**`NOTIFIO_LOG_LEVEL` is outside `config.json` on purpose.** If the config file will not parse,
a level written inside it is unreachable at exactly the moment somebody needs `debug` to find
out why. It also means the first lines a container writes — reading the config, probing the
volume — are already at the level that was asked for. The cost is that changing it is a
restart, where a config key would have been picked up by the reload; that is the right trade
for a setting whose purpose is to work when the other mechanism is broken.

There is **no `NOTIFIO_PUBLIC_URL`**. notifio builds no links, sets no cookies and signs
nothing, so it never needs to know its own address — which also means moving it costs nothing.

## Behind caddy-docker-proxy

`docker-compose.yml` in the repository root is a working deployment. The short version:

```yaml
services:
  notifio:
    image: ghcr.io/reeywhaar/notifio:latest
    restart: unless-stopped
    volumes:
      - data:/data              # required
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

caddy gets the certificate and terminates TLS; notifio speaks plain HTTP behind it and never
learns its own hostname, so moving it to a different domain is a label change and nothing else.

## Health

```
HEALTHCHECK CMD ["notifio", "healthcheck"]
```

The binary calls itself over loopback, so the image needs no HTTP client and a process that is
running but wedged fails the check. `GET /healthz` returns `{"ok":true}`, the version, and how
many channels loaded.

It deliberately does not reach out to Telegram or the relay: a healthcheck that fails because
somebody else's server is down restarts a container that was working fine. For the same reason
it stays `200` when `config.json` stops parsing, and reports `"config":"stale"` instead — a
restart would find the same file and refuse to start at all.

## Backups

**Read this first: the archive holds every credential notifio has, in cleartext.** Bot tokens
and relay passwords live in `config.json` as you typed them — they cannot be hashed, because
notifio has to present them to the provider. **Set `BACKUP_PASSWORD` on the sidecar**, which
repacks each archive as an AES-256 zip before upload, and point it at a remote that is not
shared. An unencrypted backup of this particular file is worse than no backup.

Set `NOTIFIO_BACKUP_URL` and notifio posts a `.tgz` of `config.json` and `data.json` to a
[backio-agent](https://github.com/reeywhaar/backio/tree/main/agent) sidecar whenever either
file's contents have changed, checked every five minutes, with the first pass immediate at
startup.

**notifio sends the archive and a name, and nothing else.** No credential, no provider, no
subdirectory — those live in the sidecar, which is what stops a compromised notifio from
reading, overwriting or deleting a single existing backup. The sidecar assigns the stored
filename; `name` is read for its extension only.

The digest is over the files' contents, not their mtimes, so a restart that rewrites nothing
sends nothing, and neither does an editor that saves identical bytes. The five-minute interval
is a throttle as much as a delay: four tokens minted in a minute produce one archive holding
all four.

A failed push is logged at `ERROR` and never fatal, and is not retried inside the pass — what
fails is either transient or needs a person, and the next pass is in five minutes.

`notifio backup now` sends one unconditionally.

### Restore

There is no `restore` command, which is the right amount of code for a two-file archive:

```sh
docker compose stop notifio
docker run --rm -v notifio_data:/data -v "$PWD":/in alpine tar xzf /in/notifio.tgz -C /data
docker compose start notifio
```

A restore command would be a rarely-run path that overwrites live credentials, which is the
worst combination of properties a command can have.

## What the logs contain

`log/slog`, JSON, on stdout. One line per send:

```json
{"time":"2026-09-12T04:00:00Z","level":"INFO","msg":"sent","token":"grafana",
 "channel":"alerts","type":"telegram","to":"…7890","body_type":"md","attachments":2,
 "status":200,"dur_ms":312,"id":"4821","client":"10.0.0.7"}
```

Everything it writes, and when:

| level | `msg` | when |
| --- | --- | --- |
| INFO | `listening` | startup, with the channels it loaded |
| WARN | `no tokens exist` | startup, when nothing can send yet |
| DEBUG | `sending` | a send began — so one that never comes back is visible as something that started |
| INFO | `sent` | a send succeeded |
| WARN | `refused` | the request was rejected: `400`, `413`, `415`, `422` |
| WARN | `rejected` | an unknown token, with the client address and no token material |
| ERROR | `send failed` | the provider refused it or could not be reached |
| INFO | `tokens changed` | `data.json` changed under the process — see below |
| INFO | `channels changed` | a channel was added to or removed from `config.json` |
| ERROR | `config will not load` | once when it breaks, and `config loaded again` once when it recovers |
| ERROR | `orphaned token` | a token whose channel is no longer configured |
| INFO | `backing up` / `backed up` | the backup loop, when a URL is set |
| ERROR | `backup failed` | a push the agent refused |

**`tokens changed` is the one audit line.** `docker exec notifio notifio token add` is a second
process, so the server's log is the only place a minted or withdrawn credential can be noticed
at all. It carries the count and each `label@channel`, never a secret. The store notices the
file when something reads it, and `/healthz` does — so a change appears within a healthcheck
interval rather than whenever the next send happens to arrive.

**Nothing logs a `channel test`.** That command is a separate process and its output goes to
whoever ran it; the server has no way to know a message left through it.

### `client`, behind a proxy

Behind caddy the peer address is caddy's — `172.19.0.3` and friends — which is no use in a log.
notifio reads `X-Forwarded-For`, then `X-Real-IP`, to get the caller's own.

**Those headers are believed only when the machine that handed us the request is itself on the
loopback or a private network.** That is where a reverse proxy in a compose file sits, and it is
not where the internet is. Run notifio with a port published straight to a public address and
the headers are ignored entirely, so nobody can write their own address into your log.

**Of `X-Forwarded-For`, the rightmost entry wins.** That is the address the nearest proxy
actually observed; everything to its left was supplied by the caller and may be invented. Taking
the leftmost is the usual mistake, and it is the spoofable one — a caller who sends
`X-Forwarded-For: 1.2.3.4` ends up with `1.2.3.4, <their real address>` once caddy appends what
it saw, and notifio logs the second.

Two consequences worth knowing:

- **If your proxy sits on a public address**, forwarded headers are ignored and every line shows
  the proxy. Put it on the same private network as notifio, which is the ordinary arrangement.
- **If your proxy does not set either header**, every line shows the proxy's private address,
  which is true and useless. caddy and traefik set them by default; nginx wants
  `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`.

There is no list of trusted proxies to configure. Three variables do not need a fourth.

What is deliberately **not** in it, enforced by the code and asserted by a test:

- **Never the body.** Not truncated, not at `debug`. A notification body is the message itself —
  a password reset link, a customer's name, an internal hostname — and a log is the one place
  it was not meant to go.
- **Never the subject**, for the same reason. It is a body with a shorter length limit.
- **Never a token**, sent or rejected, and not a prefix of one either.
- **Never a channel credential.** The config structs redact under `%v`, so a format verb
  written in a hurry cannot leak a bot token.
- **`to` is truncated** — the last four characters for Telegram, the domain for email. Enough to
  tell two destinations apart in an incident, not enough to be a recipient list. The full value
  is at `debug`, which is a deliberate opt-in.

Auth failures log at `WARN` with the client address and no token material. Provider failures
log at `ERROR` with the provider's own message.

## CI

`.github/workflows/publish.yml` on push to `main`: `test` (gofmt, vet, `go test`) gates
`publish` (buildx, both architectures, GHCR), and `notify` reports either outcome — **through
notifio itself**.

### The notify action

`ghactions/notify` is a composite action in this repository. Two secrets:

| secret | |
| --- | --- |
| `NOTIFIO_HOST` | e.g. `https://notify.example.com` |
| `NOTIFIO_TOKEN` | a token issued for the channel these land in. A nonced one (`nts_…`) is signed rather than sent — see [nonced.md](nonced.md#from-github-actions) |

Nothing in the workflow says where a notification goes — the token names the channel and the
channel pins its destination, so moving the notifications elsewhere is a config edit on the
notifio instance and no change here.

A notifier channel wants `"pinned": { "to": "…", "link_preview": false }`: build messages are
mostly links, and a preview card over each one is noise. Pinning it beats passing it per call,
and it means the same workflow works against an email channel, which has no such field.

Other repositories can use it too:

```yaml
- uses: reeywhaar/notifio/ghactions/notify@main
  with:
    host: ${{ secrets.NOTIFIO_HOST }}
    token: ${{ secrets.NOTIFIO_TOKEN }}
    body: "<b>${{ github.repository }}</b> deployed"
```

It prints **nothing on success** — the step's own result already says whether it worked, and
the answer carries a channel name and a message id that a build log has no reason to keep. A
failure prints notifio's error body in full, because that is the only thing that says what to
fix.

There is no flag to turn that around. The message id is available as `steps.<id>.outputs.id`,
which is where something a later step might want belongs, and which no log sees.

### What a CI log still shows

**The body itself is in the log regardless**, because GitHub echoes a step's `with:` inputs
before running it. No action can prevent that.

What it does mask is **secrets**: `${{ secrets.NOTIFIO_HOST }}` and `${{ secrets.NOTIFIO_TOKEN }}`
appear as `***` wherever they are printed. So if part of a notification must not appear in a
public build log, it has to come from a secret rather than be assembled in the workflow file.

The action is a thin wrapper around `ghactions/notify/notify.sh`, and the smoke test runs that
script rather than a copy of it, so what CI sends is what was exercised against a real
instance.

The smoke assertions live in `.github/smoke.sh` so they can be run against a local build:

```sh
docker build -t notifio:test .
IMAGE=notifio:test .github/smoke.sh
```

It brings up maildev and a stand-in for the backup sidecar, and asserts the things no unit test
can: that a container with no volume refuses to start, that a non-ASCII subject and filename
survive the whole path, that a header-injection attempt delivers nothing, and that no secret
reaches the log.

The Telegram path has no throwaway bot to send to and is covered by the `httptest` units in
`internal/channel/telegram` instead.

## Development

Three variables the binary accepts and the image does not expose:

| variable | for |
| --- | --- |
| `NOTIFIO_CONFIG` | Pointing a local run at a temporary config |
| `NOTIFIO_DATA_DIR` | The same, for `data.json` |
| `PORT` | Running without a container |

```sh
mkdir -p /tmp/notifio && cp config.example.json /tmp/notifio/config.json
NOTIFIO_DATA_DIR=/tmp/notifio NOTIFIO_CONFIG=/tmp/notifio/config.json PORT=8080 go run . serve
```

These are not knobs in a container — moving `config.json` out of the volume it is mounted in
produces a notifio whose config is not backed up, which is a way to lose credentials and not a
configuration option. **The binary's surface is larger than the image's, and only the image's
is a promise.**
