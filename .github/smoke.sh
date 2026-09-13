#!/bin/sh
# Smoke-test a built notifio image end to end.
#
# Run from the repository root with IMAGE set:
#
#     IMAGE=notifio:test .github/smoke.sh
#
# It is a file rather than a block in publish.yml so it can be run locally against a local
# build, which is the only way these assertions get exercised before they run in CI.
set -eu

IMAGE="${IMAGE:?IMAGE is required}"
NET=notifio-smoke-net
VOL=notifio-smoke-data
PORT="${PORT:-8080}"
B="http://127.0.0.1:$PORT/api/send"

cleanup() {
	docker rm -f notifio maildev sink >/dev/null 2>&1 || true
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker volume rm -f "$VOL" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

step() { printf '\n== %s\n' "$1"; }
die() { printf '   FAIL %s\n' "$1"; exit 1; }

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
want() {
	expected="$1"
	shift
	got=$(code "$@")
	[ "$got" = "$expected" ] || die "expected $expected, got $got"
	printf '   ok   %s\n' "$expected"
}

maildev() { docker run --rm --network "$NET" curlimages/curl -sS "http://maildev:1080$1"; }
mailcount() { maildev /api/email | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }

step "version runs in a bare container"
docker run --rm "$IMAGE" version

step "no volume is a refusal, and says which"
if docker run --rm "$IMAGE" serve 2>/tmp/nomount; then die "started with no /data"; fi
grep -q "mount a volume" /tmp/nomount || die "the message does not say to mount a volume"
printf '   ok   %s' "$(cat /tmp/nomount)"

step "a volume with no config is a different refusal"
docker volume create "$VOL" >/dev/null
if docker run --rm -v "$VOL":/data "$IMAGE" serve 2>/tmp/noconfig; then die "started with no config"; fi
grep -q "config.json does not exist" /tmp/noconfig || die "the message does not name config.json"
printf '   ok   %s' "$(cat /tmp/noconfig)"

step "bring up maildev, the backup sink, and notifio"
docker network create "$NET" >/dev/null
docker run -d --name maildev --network "$NET" maildev/maildev:latest >/dev/null
docker run -d --name sink --network "$NET" \
	-v "$PWD/.github/smoke-sink.py":/sink.py python:3-alpine python /sink.py >/dev/null

python3 - >/tmp/config.json <<'PY'
import json
print(json.dumps({
    "version": 1,
    "channels": {
        "notices": {"type": "email", "host": "maildev", "port": 1025, "encryption": "none",
                    "pinned": {"from": "Notifio <notifio@example.com>"}},
        "alerts": {"type": "telegram", "token": "123456:AAE-not-a-real-bot",
                   "pinned": {"to": "-1001234567890"}},
        "tiny": {"type": "email", "host": "maildev", "port": 1025, "encryption": "none",
                 "pinned": {"from": "n@example.com"}, "max_body": "512"},
        "fixed": {"type": "email", "host": "maildev", "port": 1025, "encryption": "none",
                  "pinned": {"from": "n@example.com", "to": "fixed@example.com"}},
    },
}, indent=2))
PY
docker run --rm -v "$VOL":/data -v /tmp/config.json:/tmp/c.json --entrypoint sh "$IMAGE" \
	-c 'cp /tmp/c.json /data/config.json'

docker run -d --name notifio --network "$NET" -v "$VOL":/data -p "$PORT":80 \
	-e NOTIFIO_BACKUP_URL=http://sink:8080/backup "$IMAGE" >/dev/null

# Wait for both, not just notifio. notifio answers in milliseconds and maildev takes seconds,
# so polling only the one that is ready first is how the first send gets a 502 on a cold runner
# and passes on a warm laptop.
wait_for() {
	i=0
	while [ "$i" -lt 60 ]; do
		if eval "$2" >/dev/null 2>&1; then
			printf '   ok   %s is up\n' "$1"
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	die "$1 never came up"
}
wait_for notifio 'curl -fsS "http://127.0.0.1:$PORT/healthz"'
wait_for maildev 'maildev /api/email'
# The HTTP API answers before the SMTP listener does, so prove the port itself accepts a
# connection before anything tries to send through it.
wait_for "maildev smtp" 'docker run --rm --network "$NET" busybox sh -c "nc -z maildev 1025"'
wait_for sink 'docker logs sink 2>&1 | grep -q . || docker run --rm --network "$NET" busybox sh -c "nc -z sink 8080"' 

step "healthz reports the channels it loaded"
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"ok":true' || die "healthz is not ok"
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"channels":4' || die "wrong channel count"
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"tokens":0' || die "healthz does not report the token count"
printf '   ok   %s\n' "$(curl -fsS "http://127.0.0.1:$PORT/healthz")"

step "an instance with no tokens says so rather than refusing in silence"
docker logs notifio 2>&1 | grep -q "no tokens exist" || die "no warning about having no tokens"

step "tokens are minted for a channel, and only for one that exists"
TOK=$(docker exec notifio notifio token add smoke notices)
TG=$(docker exec notifio notifio token add tg alerts)
SMALL=$(docker exec notifio notifio token add small tiny)
case "$TOK" in
nt_*) ;;
*) die "token add printed something other than a token: $TOK" ;;
esac
if docker exec notifio notifio token add bad nosuchchannel 2>/dev/null; then
	die "a token was minted for an unknown channel"
fi
docker exec notifio notifio token list | grep -q "smoke.*notices" || die "token list does not show the channel"

step "listings never carry a credential"
docker exec notifio notifio channel list | grep -q "AAE-not-a-real-bot" && die "channel list printed a bot token"
docker exec notifio notifio channel list --json | grep -q "AAE-not-a-real-bot" && die "channel list --json printed a bot token"
printf '   ok   no credential in either format\n'

step "auth"
want 401 -X POST "$B" --data-urlencode 'body=x'
want 401 -X POST "$B" -H "Authorization: Bearer nt_wrong" --data-urlencode 'body=x'

step "a urlencoded send arrives"
want 200 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=ops@example.com' --data-urlencode 'subject=Disk at 91%' \
	--data-urlencode 'body=it is full'
sleep 2
maildev /api/email | grep -q 'Disk at 91%' || die "the message did not arrive"

step "the delivered message carries the headers notifio generates"
ID=$(maildev /api/email | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
maildev "/api/email/$ID/source" >/tmp/raw.eml
for h in "Date:" "Message-ID:" "MIME-Version:"; do
	grep -q "$h" /tmp/raw.eml || die "the delivered message has no $h"
	printf '   ok   %s\n' "$h"
done

step "a non-ASCII subject and filename survive the parser and the encoder alike"
printf 'id,name\n1,Дом\n' >/tmp/report.csv
want 200 -X POST "$B" -H "Authorization: Bearer $TOK" \
	-F 'to=ops@example.com' -F 'subject=Отчёт готов' -F 'body=see attached' \
	-F 'attachments=@/tmp/report.csv;filename=отчёт 2026.csv;type=text/csv'
sleep 2
maildev /api/email | python3 -c '
import json, sys
ms = json.load(sys.stdin)
m = [x for x in ms if x["subject"] == "Отчёт готов"]
assert m, "the non-ASCII subject did not arrive decoded"
names = [a["filename"] for a in m[0].get("attachments") or []]
assert names == ["отчёт 2026.csv"], f"the attachment arrived as {names}"
print("   ok   subject and filename round-tripped")
'

step "a line break in a header is refused, and nothing is delivered"
before=$(mailcount)
want 400 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=a@example.com' --data-urlencode "$(printf 'subject=hi\r\nBcc: evil@example.com')" \
	--data-urlencode 'body=x'
[ "$before" = "$(mailcount)" ] || die "a header-injection attempt was delivered"

step "a field that would silently do nothing is refused"
# `from` has nowhere to go on a telegram message, and `channel` was already decided by the
# token. Both are refused rather than dropped, because a field that does nothing teaches a
# caller that it does something.
want 400 -X POST "$B" -H "Authorization: Bearer $TG" \
	--data-urlencode 'from=a@example.com' --data-urlencode 'body=x'
want 400 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=a@example.com' --data-urlencode 'subject=s' \
	--data-urlencode 'body=x' --data-urlencode 'channel=notices'

step "a field a type simply cannot use is absorbed, not refused"
# The other half of the rule: a generic sender cannot know which kind of channel its token
# points at, and should not lose a notification over that.
want 200 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=a@example.com' --data-urlencode 'subject=s' \
	--data-urlencode 'body=x' --data-urlencode 'link_preview=false'

step "a pinned channel refuses a redirect but accepts a restatement"
# `alerts` pins `to`, so a request cannot send it anywhere else.
want 400 -X POST "$B" -H "Authorization: Bearer $TG" \
	--data-urlencode 'to=-1009999999999' --data-urlencode 'body=x'
# ...but restating it is not contradicting it. 502 rather than 200 because this channel's bot
# is not real: getting past validation to the provider is the whole assertion.
want 502 -X POST "$B" -H "Authorization: Bearer $TG" \
	--data-urlencode 'to=-1001234567890' --data-urlencode 'body=x'
docker exec notifio notifio channel test alerts --to -100999 2>&1 | grep -q pins \
	|| die "channel test --to was accepted on a pinned channel"
printf '   ok   a differing value is refused, the same value is not\n'

# The same rule on email, for both fields it can pin.
FIXED=$(docker exec notifio notifio token add fixed fixed)
want 400 -X POST "$B" -H "Authorization: Bearer $FIXED" \
	--data-urlencode 'to=elsewhere@example.com' --data-urlencode 'subject=s' --data-urlencode 'body=x'
want 400 -X POST "$B" -H "Authorization: Bearer $FIXED" \
	--data-urlencode 'from=someone@example.com' --data-urlencode 'subject=s' --data-urlencode 'body=x'
want 200 -X POST "$B" -H "Authorization: Bearer $FIXED" \
	--data-urlencode 'to=fixed@example.com' --data-urlencode 'from=n@example.com' \
	--data-urlencode 'subject=restated' --data-urlencode 'body=x'
want 200 -X POST "$B" -H "Authorization: Bearer $FIXED" \
	--data-urlencode 'subject=pinned both ways' --data-urlencode 'body=x'
sleep 2
maildev /api/email | grep -q 'fixed@example.com' || die "the pinned recipient did not receive it"
printf '   ok   a fully pinned channel takes a subject and a body and nothing else\n'

step "channel test sends a marked-up message through the real provider"
docker exec notifio notifio channel test notices --to ops@example.com >/dev/null
sleep 2
ID=$(maildev /api/email | python3 -c '
import json, sys
m = [x for x in json.load(sys.stdin) if x["subject"] == "notifio test"]
assert m, "the test message did not arrive"
html = m[0].get("html") or ""
assert "<b>notifio test</b>" in html, f"it arrived without its markup: {html[:200]}"
print(m[0]["id"])
')
maildev "/api/email/$ID/source" | grep -q "Content-Type: text/html" \
	|| die "the marked-up test message was not sent as text/html"
printf '   ok   arrived as text/html with its markup intact\n'

docker exec notifio notifio channel test notices --to plain@example.com --body-type plain >/dev/null
sleep 2
PID=$(maildev /api/email | python3 -c '
import json, sys
m = [x for x in json.load(sys.stdin) if x["to"][0]["address"] == "plain@example.com"]
assert m, "the plain test message did not arrive"
assert not (m[0].get("html") or ""), "--body-type plain still sent HTML"
print(m[0]["id"])
')
maildev "/api/email/$PID/source" | grep -q "Content-Type: text/plain" \
	|| die "--body-type plain was not sent as text/plain"
printf '   ok   --body-type plain sends text/plain and no markup\n'

step "a token reaches its own channel and no other"
before=$(mailcount)
want 502 -X POST "$B" -H "Authorization: Bearer $TG" --data-urlencode 'body=x'
[ "$before" = "$(mailcount)" ] || die "a telegram token delivered mail"

step "the body cap is the channel's own"
BIG=$(head -c 2000 /dev/zero | tr '\0' 'x')
want 413 -X POST "$B" -H "Authorization: Bearer $SMALL" \
	--data-urlencode 'to=a@example.com' --data-urlencode 'subject=s' --data-urlencode "body=$BIG"
want 200 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=a@example.com' --data-urlencode 'subject=s' --data-urlencode "body=$BIG"

step "a content type notifio does not parse"
want 415 -X POST "$B" -H "Authorization: Bearer $TOK" -H 'Content-Type: text/plain' -d 'x'

step "nothing anybody sent is in the log"
for secret in "$TOK" "$TG" "$SMALL" "it is full" "Отчёт готов" "ops@example.com"; do
	if docker logs notifio 2>&1 | grep -qF "$secret"; then die "the log carries: $secret"; fi
done
docker logs notifio 2>&1 | grep -q '"token":"smoke"' || die "the log does not carry the token label"
printf '   ok   labels yes, secrets no\n'

step "the notify action sends through a running notifio"
# The same script the composite action runs, so what CI uses is what was exercised here.
#
# `fixed` pins both `to` and `from`, which is the shape a notifier channel wants: the action
# carries an address, a credential and a message, and says nothing about where it lands.
NOTIFIO_HOST="http://127.0.0.1:$PORT" \
	NOTIFIO_TOKEN="$FIXED" \
	NOTIFIO_SUBJECT="notifio published" \
	NOTIFIO_BODY="🔔 **notifio** published \`ghcr.io/reeywhaar/notifio:latest\`" \
	ghactions/notify/notify.sh
sleep 2
maildev /api/email | python3 -c '
import json, sys
m = [x for x in json.load(sys.stdin) if x["subject"] == "notifio published"]
assert m, "the action did not deliver"
html = m[0].get("html") or ""
assert "<strong>notifio</strong>" in html, f"the action lost its markup: {html[:200]!r}"
assert "<code>ghcr.io/reeywhaar/notifio:latest</code>" in html, "code span not rendered"
print("   ok   the action delivered, its markdown rendered")
'
# It has to fail loudly rather than report success on a refusal.
if NOTIFIO_HOST="http://127.0.0.1:$PORT" NOTIFIO_TOKEN=nt_wrong NOTIFIO_BODY=x \
	ghactions/notify/notify.sh >/dev/null 2>&1; then
	die "the action reported success on a 401"
fi
printf '   ok   a refusal fails the step\n'

# The action can set link_preview without knowing the channel type: email ignores it.
NOTIFIO_HOST="http://127.0.0.1:$PORT" NOTIFIO_TOKEN="$FIXED" NOTIFIO_SUBJECT="ignored flag" \
	NOTIFIO_BODY=x NOTIFIO_LINK_PREVIEW=false \
	ghactions/notify/notify.sh >/dev/null \
	|| die "an email channel refused link_preview instead of ignoring it"
printf '   ok   link_preview is ignored on an email channel\n'

step "md is CommonMark on both channel types"
# One body, two renderings. The email one is asserted here; the telegram renderer is covered by
# internal/markup, which needs no bot.
want 200 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=md@example.com' --data-urlencode 'subject=markdown' \
	--data-urlencode 'body_type=md' \
	--data-urlencode 'body=**bold** and `code` and [a link](https://example.com)

- one
- two'
sleep 2
maildev /api/email | python3 -c '
import json, sys
m = [x for x in json.load(sys.stdin) if x["subject"] == "markdown"]
assert m, "the markdown message did not arrive"
html = m[0].get("html") or ""
for want in ("<strong>bold</strong>", "<code>code</code>", "<li>one</li>"):
    assert want in html, f"markdown was not rendered: {want!r} missing from {html[:200]!r}"
assert "**bold**" not in html, "the markdown was passed through unrendered"
print("   ok   rendered to html on the way to the relay")
'

step "a subject a telegram channel cannot carry becomes a title"
# 502 rather than 400: it passed validation and reached a bot that is not real. Before this it
# was a 400, and a CI job that could not know the channel type lost its notification.
want 502 -X POST "$B" -H "Authorization: Bearer $TG" \
	--data-urlencode 'subject=Deploy finished' --data-urlencode 'body=x'
# ...and an email channel with no subject at all still sends.
want 200 -X POST "$B" -H "Authorization: Bearer $FIXED" --data-urlencode 'body=no subject here'
sleep 2
maildev /api/email | python3 -c '
import json, sys
m = [x for x in json.load(sys.stdin) if x["subject"] == "No subject"]
assert m, "a subjectless email did not get the default"
print("   ok   telegram takes a title, email defaults the subject")
'

step "a forwarded address reaches the log, and a spoofed one does not"
# The container is reached over the docker bridge, which is private, so the header is believed.
want 200 -X POST "$B" -H "Authorization: Bearer $FIXED" \
	-H 'X-Forwarded-For: 1.2.3.4, 198.51.100.7' \
	--data-urlencode 'subject=forwarded' --data-urlencode 'body=x'
docker logs notifio 2>&1 | grep -q '"client":"198.51.100.7"' \
	|| die "the forwarded address is not in the log"
docker logs notifio 2>&1 | grep -q '"client":"1.2.3.4"' \
	&& die "the caller's invented address was believed"
printf '   ok   the rightmost entry wins and the prefix is ignored\n'

step "a nonced token sends without its secret crossing the wire"
NSECRET=$(docker exec notifio notifio token add nonced fixed --nonced)
# Derived from the secret, not looked up: a caller is given one value, same as a bearer token.
NID=$(printf %s "$NSECRET" | sha256sum | cut -c1-8)
docker exec notifio notifio token list --json \
	| grep -q "\"id\": \"$NID\"" || die "the derived id does not match the server's" 
case "$NSECRET" in
nts_*) ;;
*) die "token add --nonced printed something other than a nonced secret" ;;
esac

TS=$(date +%s)
WIRE="ntc_$TS.$NID.$(printf '%s.%s.%s' "$TS" "$NID" "$NSECRET" | sha256sum | cut -d' ' -f1)"
want 200 -X POST "$B" -H "Authorization: Bearer $WIRE" \
	--data-urlencode 'subject=nonced' --data-urlencode 'body=x'

# The secret itself must not work as a bearer token, or the protection would be optional.
want 401 -X POST "$B" -H "Authorization: Bearer $NSECRET" \
	--data-urlencode 'subject=s' --data-urlencode 'body=x'

# And a captured value stops working once the window passes.
OLD=$((TS - 600))
STALE="ntc_$OLD.$NID.$(printf '%s.%s.%s' "$OLD" "$NID" "$NSECRET" | sha256sum | cut -d' ' -f1)"
want 401 -X POST "$B" -H "Authorization: Bearer $STALE" \
	--data-urlencode 'subject=s' --data-urlencode 'body=x'

docker logs notifio 2>&1 | grep -q '"auth":"nonced"' || die "the log does not record the auth kind"
for secret in "$NSECRET"; do
	if docker logs notifio 2>&1 | grep -qF "$secret"; then die "the nonced secret is in the log"; fi
done
printf '   ok   sent by hash, secret refused as bearer, stale nonce refused\n'

# The same action, handed a nonced secret: it signs rather than sending it, so migrating is
# swapping the secret with no workflow change.
NOTIFIO_HOST="http://127.0.0.1:$PORT" NOTIFIO_TOKEN="$NSECRET" \
	NOTIFIO_SUBJECT="published, nonced" NOTIFIO_BODY="🔔 **notifio** published" \
	ghactions/notify/notify.sh >/dev/null \
	|| die "the action could not send with a nonced secret"
docker logs notifio 2>&1 | grep -q '"token":"nonced","auth":"nonced"' \
	|| die "the action did not authenticate as nonced"
docker logs notifio 2>&1 | grep -qF "$NSECRET" && die "the action put the nonced secret on the wire"
printf '   ok   the same action signs a nonced secret and sends a bearer one\n'

step "a credential minted by a second process reaches the log"
# docker exec is a different process, so this line is the only trace the server has.
docker exec notifio notifio token add audited notices >/dev/null
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null
docker logs notifio 2>&1 | grep -q '"msg":"tokens changed"' || die "a minted token was not logged"
docker logs notifio 2>&1 | grep -q 'audited@notices' || die "the line does not name the token and channel"
docker exec notifio notifio token remove audited >/dev/null
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null
n=$(docker logs notifio 2>&1 | grep -c '"msg":"tokens changed"' || true)
[ "$n" -ge 2 ] || die "the withdrawal was not logged ($n lines)"
printf '   ok   mint and withdrawal both recorded, with no secret\n'

step "the backup carries an archive and a name, and no authority"
docker exec notifio notifio backup now
docker logs sink 2>&1 | grep -q 'AUTH=None' || die "notifio sent a credential the sidecar does not take"
docker logs sink 2>&1 | grep -q 'FIELDS=backup,name' || die "notifio sent fields the sidecar ignores"
printf '   ok   %s\n' "$(docker logs sink 2>&1 | tail -1)"

step "a withdrawn token stops working, with no restart"
docker exec notifio notifio token remove smoke
want 401 -X POST "$B" -H "Authorization: Bearer $TOK" \
	--data-urlencode 'to=a@example.com' --data-urlencode 'subject=s' --data-urlencode 'body=x'

step "a broken config keeps serving, and says so"
# The copy lives in the volume, not in /tmp: each docker run is its own container.
docker run --rm -v "$VOL":/data python:3-alpine sh -c \
	'cp /data/config.json /data/config.good && printf "{ broken" > /data/config.json'

H=$(curl -fsS "http://127.0.0.1:$PORT/healthz")
echo "$H" | grep -q '"config":"stale"' || die "healthz does not report a stale config: $H"
echo "$H" | grep -q '"channels":4' || die "healthz forgot the channels it is still serving: $H"

# Still sending, on the channels it already had.
want 200 -X POST "$B" -H "Authorization: Bearer $FIXED" \
	--data-urlencode 'subject=still working' --data-urlencode 'body=x'

# Once, not once per healthcheck.
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null
n=$(docker logs notifio 2>&1 | grep -c "config will not load" || true)
[ "$n" = 1 ] || die "the failure was logged $n times, want exactly 1"

docker run --rm -v "$VOL":/data python:3-alpine sh -c \
	'mv /data/config.good /data/config.json'
curl -fsS "http://127.0.0.1:$PORT/healthz" | grep -q '"config"' \
	&& die "healthz still reports a stale config after it was fixed"
docker logs notifio 2>&1 | grep -q "config loaded again" || die "recovery was not logged"
printf '   ok   served through it, logged once, and reported the recovery\n'

step "a token whose channel was removed is 503, not 401"
docker run --rm -v "$VOL":/data python:3-alpine python -c '
import json
c = json.load(open("/data/config.json"))
del c["channels"]["alerts"]
json.dump(c, open("/data/config.json", "w"))
'
sleep 1
want 503 -X POST "$B" -H "Authorization: Bearer $TG" --data-urlencode 'body=x'

printf '\nall good\n'
