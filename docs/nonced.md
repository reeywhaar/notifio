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
same as a bearer token.

The id is on the wire because the server has to know *which* secret to check before it can
check anything. It is not a second credential: it is public, it is in `notifio token list`, and
it identifies without revealing.

### bash

`sha256sum` is in every base image including busybox, which `openssl` is not — that is why this
is a plain hash and not an HMAC.

```sh
notifio_auth() {
	case "$1" in
	nts_*) ;;
	*) printf %s "$1"; return ;;          # a bearer token goes as-is
	esac
	id=$(printf %s "$1" | sha256sum | cut -c1-8)
	ts=$(date +%s)
	printf 'ntc_%s.%s.%s' "$ts" "$id" \
		"$(printf '%s.%s.%s' "$ts" "$id" "$1" | sha256sum | cut -d' ' -f1)"
}

curl -X POST https://notify.example.com/api/send \
	-H "Authorization: Bearer $(notifio_auth "$SECRET")" \
	--data-urlencode 'body=…'
```

### TypeScript

Web Crypto, so it runs unchanged in Node, Deno, Bun and a worker — no dependency.

```ts
const enc = new TextEncoder()

const sha256 = async (s: string): Promise<string> =>
  [...new Uint8Array(await crypto.subtle.digest("SHA-256", enc.encode(s)))]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("")

/** Returns what goes after `Bearer `. A bearer token is returned unchanged. */
export async function notifioAuth(secret: string): Promise<string> {
  if (!secret.startsWith("nts_")) return secret
  const id = (await sha256(secret)).slice(0, 8)
  const ts = Math.floor(Date.now() / 1000)
  return `ntc_${ts}.${id}.${await sha256(`${ts}.${id}.${secret}`)}`
}

await fetch("https://notify.example.com/api/send", {
  method: "POST",
  headers: {
    authorization: `Bearer ${await notifioAuth(process.env.NOTIFIO_TOKEN!)}`,
    "content-type": "application/x-www-form-urlencoded",
  },
  body: new URLSearchParams({ body: "…" }),
})
```

Both recipes take either kind of secret and branch on its prefix, so a caller does not need to
know which it was given — which is what makes migrating one a matter of swapping a value.

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

## From GitHub Actions

`ghactions/notify` handles both kinds and picks by the secret's own prefix, so **migrating is
swapping the secret** — the workflow does not change:

```yaml
- uses: reeywhaar/notifio/ghactions/notify@main
  with:
    host: ${{ secrets.NOTIFIO_HOST }}
    token: ${{ secrets.NOTIFIO_TOKEN }}     # nt_… sent as-is, nts_… signed
    body: "🔔 **${{ github.repository }}** deployed"
```

It needs `sha256sum` on the runner, which the hosted images have; if it is missing the step
fails saying so rather than falling back to sending the secret.

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

## Standards, and why none of them

There is no RFC for exactly this shape, and the ones that come close were each ruled out for a
reason worth keeping written down — otherwise they get re-proposed every few months.

| | what it is | why not |
| --- | --- | --- |
| **RFC 9421** *HTTP Message Signatures* | The general form of this. Covers chosen components with `created`, `keyid` and `alg`; a covered set of `("@method" "@target-uri")` with no body digest is the same idea properly spelled | **Too much machinery for the job.** Structured-field parsing, derived components, a canonical signature base. A caller would need a library, which defeats the point of a value you can build in two lines of shell. If notifio ever covers the body, this is what it should become |
| **RFC 9530** *Digest Fields* | `Content-Digest`, how you bind a body into the above | Only needed if the body is covered, and covering the body would mean reading it before authenticating — see [What it does not do](#what-it-does-not-do) |
| **RFC 6238** *TOTP* | Literally HMAC over a time counter | Built for six digits a human types in thirty seconds. Truncation is the whole point of it and is exactly what you do not want here; untruncated with a five-minute step it *is* this scheme, just described in a spec about authenticator apps |
| **RFC 7616** *HTTP Digest Access Auth* | Nonce-based challenge-response | The nonce is **server-issued**, so every send costs a round trip to fetch a challenge first. Built for passwords, and more ceremony than RFC 9421 rather than less |
| **RFC 5849** *OAuth 1.0a* | `oauth_timestamp` + `oauth_nonce` + HMAC-SHA1 | The direct ancestor, and abandoned in favour of bearer-over-TLS. Its signature base — parameter sorting, double percent-encoding — was notorious, and it is the specific mistake this stays clear of |
| **JWT (RFC 7519) with a short `exp`** | An HS256 token that expires | Would work, and gives the same five-minute property. But a JWT *is* a bearer token, so a caller has to mint and carry one; and it drags in base64url of two JSON objects, plus the `alg` confusion pitfalls, to express one timestamp |
| **HMAC instead of SHA-256** | The textbook keyed MAC | Needs `openssl(1)`, which is in **none** of alpine, debian-slim or ubuntu. The length-extension argument it defends against [does not apply](#why-plain-sha-256-is-enough) when the secret goes last |
| **mTLS** | Client certificates | Genuinely stronger and genuinely more operational work: a CA, issuance, rotation, and reverse-proxy configuration. Wrong size for a tool whose selling point is a curl one-liner |

The closest thing in daily use is the webhook-signing convention — Stripe's `t=…,v1=…`, Slack's
`X-Slack-Request-Timestamp` plus a signature, GitHub's `X-Hub-Signature-256`. This follows that
shape, minus the body, and reuses the `Authorization` header rather than inventing one, so
everything in front of notifio keeps redacting it.

## The file version

`data.json` is written as **version 2** once it holds a nonced token, and stays version 1 until
then. That is deliberate rather than tidy: the token loader ignores fields it does not know, so
an older binary reading a version 2 file would drop `secret`, fall back to `hash`, and accept
the secret as a bearer token — the exact thing this exists to prevent. The version gate makes it
refuse the file instead.

An instance that never mints a nonced token keeps a version 1 file and stays readable by any
build.
