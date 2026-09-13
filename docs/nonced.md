# Nonced tokens

A bearer token is the credential. Send it, and anything that sees the request sees something it
can reuse forever — a log with headers turned on, a debug proxy, a paste into an issue.

A nonced token is not the credential. The caller hashes its secret together with the current
time and sends **that**; the secret stays where it is. Anything that captures the wire value
gets something that stops working in five minutes.

```
Authorization: Bearer <value>

ntc_1789311374.470c327c.03ed800cea02300d0feaf2199925b569ec80dde7be81c4534e1bebd587adcd7c
└┬─┘└────┬───┘ └───┬──┘ └───────────────────────────────┬──────────────────────────────┘
 │       │         │                                    │
 │       │         │                                    sha256 of "<nonce>.<id>.<secret>"
 │       │         token id — sha256(secret)[:8]
 │       nonce — unix seconds
 prefix
```

## The three prefixes

The kind is legible from the value itself, before any lookup:

| | what it is | where it lives |
| --- | --- | --- |
| `nt_…` | a bearer secret | `data.json`, **hashed** |
| `nts_…` | a nonced token's secret | `data.json`, **in the clear** |
| `ntc_…` | what a nonced token puts on the wire | nowhere — computed per request |

## The format

```
wire  = "ntc_" <nonce> "." <id> "." <mac>
base  =        <nonce> "." <id> "." <secret>
mac   = sha256(base), lowercase hex
```

| field | |
| --- | --- |
| `nonce` | Unix seconds. Digits only, and parsed strictly — see [below](#why-plain-sha-256-is-enough) |
| `id` | the token's id: eight lowercase hex characters, from `notifio token list` |
| `mac` | 64 lowercase hex characters |

A dot separates the fields rather than a colon: it is unreserved in a URL where a colon is a
delimiter, so the value survives being put somewhere it was not meant to go. No field can
contain one — they are digits and hex.

The value goes in the ordinary `Authorization: Bearer` header. There is no second auth scheme
and no second code path; notifio dispatches on the `ntc_` prefix, and caddy still redacts the
header exactly as before.

## Making one

**The secret is the only thing a caller needs.** The id is the first eight characters of
`sha256(secret)`, so the caller derives it rather than being told it — one value to configure,
same as a bearer token:

```sh
ID=$(printf %s "$SECRET" | sha256sum | cut -c1-8)
TS=$(date +%s)
WIRE="ntc_$TS.$ID.$(printf '%s.%s.%s' "$TS" "$ID" "$SECRET" | sha256sum | cut -d' ' -f1)"

curl -X POST https://notify.example.com/api/send \
  -H "Authorization: Bearer $WIRE" \
  --data-urlencode 'body=…'
```

`sha256sum` is in every base image including busybox, which `openssl` is not — that is why this
is a plain hash and not an HMAC.

The id is on the wire because the server has to know *which* secret to check before it can
check anything. It is not a second credential: it is public, it is in `notifio token list`, and
it identifies without revealing.

## Minting

```sh
notifio token add ci alerts --nonced
```

```
notifio: minted nonced token "ci" (id 470c327c) for channel "alerts". It is not shown again.
notifio: send it as  Authorization: Bearer ntc_<unix>.470c327c.<sha256 of "<unix>.470c327c.<secret>">
```

The secret goes to stdout alone, so `SECRET=$(…)` captures exactly it. The id and the recipe go
to stderr.

## What it costs

**notifio has to keep the secret in `data.json` in the clear.** A SHA-256 cannot be verified
from a digest of itself, so the server must hold the thing it is checking against. A bearer
token stores only a hash and is unrecoverable; a nonced one is not.

So the exposure moves rather than disappearing:

| | on the wire | in `data.json` |
| --- | --- | --- |
| bearer | a reusable secret | a hash — theft yields nothing |
| nonced | useless after five minutes | **the secret, in usable form** |

Whether that is a good trade depends on which you think is likelier: a token surfacing in a log
or a paste, or someone reading the volume. Log exposure is diffuse, accumulates, and depends on
every component in the path staying configured correctly — forever, across people who were not
there when it was decided. Disk exposure is one file you have already reasoned about, next to
`config.json`'s bot tokens and relay passwords, under the same `0600`.

It also means **the backup archive now carries API credentials as well as channel ones**, which
is one more reason [`BACKUP_PASSWORD`](deploy.md#backups) is not really optional.

Only tokens minted with `--nonced` pay this. Bearer tokens are untouched.

## A nonced secret is not a bearer token

Presenting `nts_…` as `Authorization: Bearer` is a `401`, even though its hash is right there in
the file. Without that the protection would be optional, and a caller could quietly undo it by
sending the easy thing.

## Why plain SHA-256 is enough

HMAC is the usual answer, and it is the wrong trade here: it would mean `openssl` on every
caller, which is absent from alpine, debian-slim and ubuntu alike.

The objection to a plain hash is length extension — given `H(secret ‖ m)` you can compute
`H(secret ‖ m ‖ pad ‖ m')` without the secret. It does not apply here, and the reason is that
**the secret goes last**: a forged digest would be for `nonce.id.secret‖pad‖extra`, a shape the
server never builds for any input. Had the secret gone first, the forgery would only have been
stopped by parsing, which is a weaker place to stand.

Two things follow, and they are security properties rather than hygiene:

- **The nonce is parsed strictly** — digits only, bounded length, rejected otherwise. It is what
  keeps the base unambiguous.
- **The comparison is constant time**, over the hex digests.

If the base ever grows another field, revisit this: the argument above is about *this* base, and
a delimiter that could appear inside a field would make `a.bc` and `ab.c` collide.

## What it does not do

- **It does not authenticate the message.** Only the nonce and the id are covered, so someone
  who intercepts a request can change its body and the signature still checks out. If that
  matters, the covered base has to include a digest of the body — and then auth can no longer
  happen before the body is read, which is a property notifio currently has and a test pins.
- **It does not prevent replay inside the window.** A captured value works for up to five
  minutes. Stripe and Slack accept the same; a nonce cache would be state that dies on restart.
- **It does not protect the secret at the caller.** "Safe to pass around" is about the wire
  value, not about what sits in your CI secrets or on the sending host.

## Standards

There is no RFC for exactly this shape. The nearest formal relatives:

| | |
| --- | --- |
| **RFC 9421** *HTTP Message Signatures* | The general version. Covers chosen components with `created`, `keyid`, `alg`; a covered set of `("@method" "@target-uri")` and no body digest is the same idea, properly spelled. Heavier than this |
| **RFC 6238** *TOTP* | Literally HMAC over a time counter, truncated for humans to type |
| **RFC 5849** *OAuth 1.0a* | Did this in 2010 with `oauth_timestamp` and `oauth_nonce`, and was abandoned for bearer-over-TLS |

The closest thing in daily use is the webhook-signing convention — Stripe's `t=…,v1=…`, Slack's
timestamp-plus-signature — which is what this follows, minus the body.

## The file version

`data.json` is written as **version 2** once it holds a nonced token, and stays version 1 until
then. That is deliberate rather than tidy: the token loader ignores fields it does not know, so
an older binary reading a version 2 file would drop `secret`, fall back to `hash`, and accept
the secret as a bearer token — the exact thing this exists to prevent. The version gate makes it
refuse the file instead.

An instance that never mints a nonced token keeps a version 1 file and stays readable by any
build.
