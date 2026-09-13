# Nonced tokens

A token can be sent whole, or kept back and proved with a hash. Same token either way — there
is nothing to choose at mint time.

```
Authorization: Bearer nt_pdAfbGB1UR64K_e9EWezO2UXZDQyfhG5yeDuCA9eEMs   ← the credential
Authorization: Bearer ntc_1789313123.5a00d88f.aded0f32fd587cf5f1757…   ← useless in 5 minutes
```

Anything that sees the first can reuse it forever: a log with headers on, a debug proxy, a paste
into an issue. The second stops working almost immediately.

## Contents

- [The format](#the-format)
- [Making one](#making-one)
- [Why it needs no second kind of token](#why-it-needs-no-second-kind-of-token)
- [Why plain SHA-256](#why-plain-sha-256)
- [What it does not do](#what-it-does-not-do)
- [Standards, and why none of them](#standards-and-why-none-of-them)

## The format

```
ntc_1789313123.5a00d88f.aded0f32fd587cf5f175732a1c9b04e7c7dd84a2ca8f6c0b8b3e2d1a0f9e8d7c
└┬─┘└────┬───┘ └───┬──┘ └───────────────────────────────┬──────────────────────────────┘
 │       │         │                                    │
 │       │         │                                    sha256("<nonce>.<id>.<key>")
 │       │         token id — the first 8 of the key
 │       nonce — unix seconds, ±5 minutes
 prefix
```

The **key** is `sha256(secret)` — which is what `data.json` already stores for every token. The
caller has the secret and derives the same value; the server never needs the secret back.

Fields are separated by a dot, which is unreserved in a URL where a colon is a delimiter. No
field can contain one: they are digits and hex.

It goes in the ordinary `Authorization: Bearer` header, so there is no second auth scheme, and
caddy still redacts it. notifio dispatches on the `ntc_` prefix.

## Making one

The secret is the only thing a caller needs. `sha256sum` is in every base image including
busybox, which `openssl` is not.

```sh
key=$(printf %s "$SECRET" | sha256sum | cut -d' ' -f1)
id=$(printf %s "$key" | cut -c1-8)
ts=$(date +%s)
auth="ntc_$ts.$id.$(printf '%s.%s.%s' "$ts" "$id" "$key" | sha256sum | cut -d' ' -f1)"

curl -X POST https://notify.example.com/api/send \
	-H "Authorization: Bearer $auth" --data-urlencode 'body=…'
```

```ts
const sha256 = async (s: string): Promise<string> =>
  [...new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)))]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("")

export async function notifioAuth(secret: string): Promise<string> {
  const key = await sha256(secret)
  const id = key.slice(0, 8)
  const ts = Math.floor(Date.now() / 1000)
  return `ntc_${ts}.${id}.${await sha256(`${ts}.${id}.${key}`)}`
}
```

Web Crypto, so it runs unchanged in Node, Deno, Bun and a worker.
[`ghactions/notify`](../ghactions/notify/notify.sh) does the same thing and is the bash version
in use.

## Why it needs no second kind of token

The first attempt at this keyed the hash on the *secret*, which meant notifio had to keep the
secret to check it against — a second kind of token, stored in the clear, with its own prefix,
its own mint flag and a `data.json` version bump.

Keying on the hash the file already holds removes all of it. Nothing is stored that was not
stored before, every existing token works, and there is nothing to decide when minting one.

What it does mean: **`data.json` is enough to send.** An attacker who reads it can compute valid
nonces, though they still cannot recover the secret itself or use it anywhere else. Before, the
file was useless without the secret. That is the one thing this costs, and it is why
[`BACKUP_PASSWORD`](deploy.md#backups) on the sidecar matters.

## Why plain SHA-256

HMAC is the usual answer and needs `openssl(1)`, which is in none of alpine, debian-slim or
ubuntu. The objection to a plain hash is length extension — given `H(k ‖ m)` you can compute
`H(k ‖ m ‖ pad ‖ m')` — and it does not apply here because **the key goes last**: a forged
digest would be for `nonce.id.key‖pad‖extra`, a shape the server never builds.

Two things are therefore load-bearing rather than tidy: the nonce is parsed **strictly** as
digits, which is what keeps the base unambiguous; and the comparison is **constant time**.

If the base ever grows a field, revisit this — the argument is about *this* base.

## What it does not do

- **It does not authenticate the message.** Only the nonce and the id are covered, so an
  interceptor can change the body. Covering the body would mean reading it before authenticating,
  which notifio deliberately does not do.
- **It does not prevent replay inside the window.** A captured value works for up to five
  minutes, as with Stripe and Slack. A nonce cache would be state that dies on restart.
- **It does not protect the secret at the caller.** Only the wire value.

## Standards, and why none of them

No RFC covers exactly this. The near misses, and why each was left alone:

| | why not |
| --- | --- |
| **RFC 9421** *HTTP Message Signatures* | The general form of this, and the right answer if the body is ever covered. Structured fields, derived components and a canonical base mean a caller needs a library, where this needs four lines of shell |
| **RFC 6238** *TOTP* | Literally a hash over a time counter, but truncated to six digits for humans — and truncation is the part you do not want |
| **RFC 7616** *HTTP Digest Auth* | The nonce is server-issued, so every send costs a round trip to fetch a challenge |
| **RFC 5849** *OAuth 1.0a* | The direct ancestor, abandoned for bearer-over-TLS. Its signature base — parameter sorting, double encoding — is the specific mistake this avoids |
| **JWT with a short `exp`** | Would work, but a JWT *is* a bearer token, and it drags in base64url of two JSON objects plus `alg` confusion to express one timestamp |
| **HMAC** | Needs `openssl`. See [above](#why-plain-sha-256) |
| **mTLS** | Stronger, and a CA to run. Wrong size for a tool whose point is a curl one-liner |

The closest thing in daily use is the webhook-signing convention — Stripe's `t=…,v1=…`, Slack's
timestamp plus signature. This is that, minus the body, reusing a header everything already
knows to redact.
