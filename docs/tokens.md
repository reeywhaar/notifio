# Tokens

A token is a destination, not a password. It is minted for exactly one channel and can send
through no other.

```sh
notifio token add grafana alerts
```

## The token is the channel

`/api/send` has **no `channel` parameter**. The token resolves to a channel, the channel to a
type, and the type to a validator — all of it before the request body is touched.

Three things follow, and each is a reason this is the right shape rather than a restriction:

- **A leaked token leaks one channel.** With a token that could reach every channel, the blast
  radius of a credential in a CI log is every bot and every relay; with this, it is the one
  that service was already sending to.

  **On a channel that pins `to` it is one destination**, exactly: the request cannot carry a
  `to` at all, so the token reaches what the config named and nothing else. On a channel that
  does not, it bounds the credential rather than the recipient. Pin what you can — see
  [channels.md](channels.md#pinned) — and give each service its own token either way, so a
  leaked one can be withdrawn alone.
- **There is no disagreement to resolve.** A request naming a channel *and* a token bound to
  one is two sources of truth, and every answer to "they disagree" is a surprise: honouring the
  field makes the binding decorative, honouring the token makes the field a lie.
- **A caller is simpler.** The destination is configuration — one URL and one token, set once —
  rather than a string repeated at every call site.

A service that needs two channels gets two tokens. That is the intended shape, not a
workaround: `deploys` and `deploys-email` are two credentials with two labels, and revoking one
does not silence the other.

## The file

`/data/data.json`, mode `0600`:

```json
{
  "version": 1,
  "tokens": [
    { "label": "grafana", "channel": "alerts", "hash": "9f86d081884c7d65…", "created_at": 1757116800 }
  ]
}
```

`channel` is required on every row. A row without one is a load error, not a token that can
reach everything.

`version` is there so a format change is a migration rather than a crash on a field that used
to mean something else. The change it is kept for is a token carrying several channels, which
is why `channel` is a string now and would become a list then.

A missing file is **not** an error: that is a fresh volume, and it means zero tokens. Zero
tokens is a warning at startup, not a refusal — refusing to start would crash-loop the
container before anyone could `docker exec` in to mint the first one.

## Labels

**Unique across the whole file, not per channel.** Required, and `token add` refuses a
duplicate.

Unique across the file rather than per channel is deliberate: `grafana` on two channels would
make `token remove grafana` ambiguous, and the fix for that is a second argument on the command
that deletes things — the worst place to put one. Two credentials get two names, `grafana-tg`
and `grafana-mail`, and both commands stay one argument long.

There are no ids. An id exists to name a row without exposing a secret, and a label does that
better because a person chose it to mean something: `grafana` in an access log answers a
question `t_01JQ…` does not.

## Format and hashing

`nt_` followed by 32 random bytes in base64url — 43 characters, 256 bits.

Stored as **SHA-256, hex**, compared with `subtle.ConstantTimeCompare` against every stored hash
with no early exit on a match.

Not bcrypt, and the difference is the point: bcrypt exists to make a low-entropy human-chosen
secret expensive to guess, and a 256-bit random string is not guessable at any price. A slow
hash here would only slow every send.

**The secret is printed once and is not stored.** A lost token is removed and minted again.

## How a running server sees `docker exec`

`docker exec notifio notifio token add x alerts` is a **second process**. It writes the file;
the server has the old one in memory.

The server calls `stat` before each auth and re-reads when the mtime or size has moved. About a
microsecond against a send that costs hundreds of milliseconds, no dependency, and no state
where the server is stale. fsnotify would be a dependency and inotify on an overlay mount is
not something to rely on; SIGHUP works and is forgotten exactly once, by the person who then
spends an hour on why their new token is rejected.

Writes are atomic — a temp file in the same directory, fsync, rename — so a reader never sees a
half-written file.

## When a token's channel is gone

Removing a channel from `config.json` while a token points at it is a state the program has to
have an answer for. Refusing to start is wrong: it would crash-loop the container over a config
edit, taking down every *other* channel with it.

So config load logs at `ERROR` naming each orphaned token, `token list` marks it `(missing)`,
and a send with that token authenticates and then fails **`503` with `code: "config"`**. Not
`401`, which would send somebody hunting for a revoked credential that is sitting there working
fine.

`token add` checks the channel exists at mint time, so this state is only ever reachable by
editing the config afterwards.
