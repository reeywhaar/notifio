# Documentation

How this project is built, so a decision made once does not have to be re-argued.

These are rules and references, not a plan — what gets built in what order is not settled
here.

| document | what it settles |
| --- | --- |
| [sending.md](sending.md) | **The contract.** The endpoint, the three content types, auth, the field tables, every status code |
| [channels.md](channels.md) | **Every config key**, and both providers in detail |
| [tokens.md](tokens.md) | `data.json`, the one-token-one-channel binding, the hashing, the reload |
| [deploy.md](deploy.md) | Image, environment, the `/data` requirement, caddy, backups, logs, CI |
| [stack.md](stack.md) | Every dependency and why. One line long |
| [conventions.md](conventions.md) | Naming, commits, comments, Go rules, time |

## What notifio is, in three sentences

notifio takes a POST and sends it as a notification through the channel its token was issued
for — a Telegram bot or an SMTP relay. An operator names those channels in a config file; a
caller holds a token and needs to know neither protocol.

It sends and nothing else: it does not receive, poll, queue, retry, or keep any record of what
went beyond the log line.

## When these disagree with the code

The code is right and the document is stale. Fix the document in the same commit that made it
stale — a reference nobody trusts is worse than no reference, because it costs a reader the
time to find out.

That is the rule and it will be broken, which is what [meta.txt](meta.txt) is for. It records
the commit each of these was last checked against, so "what has happened since anybody read
this" is `git log <hash>..HEAD` rather than a re-read of everything.
