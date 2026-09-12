#!/bin/sh
# Send one message through a notifio instance.
#
# The action calls this, and so does the smoke test, so the thing CI runs is the thing that was
# exercised against a real instance.
#
# Env: NOTIFIO_HOST, NOTIFIO_TOKEN, NOTIFIO_BODY, and optionally NOTIFIO_BODY_TYPE,
#      NOTIFIO_SUBJECT, NOTIFIO_LINK_PREVIEW.
set -eu

: "${NOTIFIO_HOST:?NOTIFIO_HOST is required}"
: "${NOTIFIO_TOKEN:?NOTIFIO_TOKEN is required}"
: "${NOTIFIO_BODY:?NOTIFIO_BODY is required}"

# Every field goes through --data-urlencode: -d would send a % or a & in the message raw, which
# is either an invalid escape or a second field.
set -- --data-urlencode "body=$NOTIFIO_BODY" \
	--data-urlencode "body_type=${NOTIFIO_BODY_TYPE:-md}"
# Empty means "say nothing about it", which is what an email channel needs: it has neither
# field and refuses one it is sent.
if [ -n "${NOTIFIO_SUBJECT:-}" ]; then
	set -- "$@" --data-urlencode "subject=$NOTIFIO_SUBJECT"
fi
if [ -n "${NOTIFIO_LINK_PREVIEW:-}" ]; then
	set -- "$@" --data-urlencode "link_preview=$NOTIFIO_LINK_PREVIEW"
fi

# The response goes to a file rather than into the log line. Nothing is printed on success: the
# step's own result already says it worked, and the answer carries a channel name and a message
# id that a build log has no reason to keep. The id is available as a step output instead.
out=$(mktemp)
trap 'rm -f "$out"' EXIT

status=$(curl -sS -o "$out" -w '%{http_code}' -X POST \
	"${NOTIFIO_HOST%/}/api/send" \
	-H "Authorization: Bearer $NOTIFIO_TOKEN" \
	"$@")

if [ "$status" -lt 200 ] || [ "$status" -ge 300 ]; then
	# A failure prints everything: notifio's error body names the cause and is the only thing
	# worth having in the log.
	echo "notifio answered $status:" >&2
	cat "$out" >&2
	exit 1
fi

# The message id goes to the step's outputs, where a later step can use it and no log sees it.
if [ -n "${GITHUB_OUTPUT:-}" ]; then
	id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' "$out")
	echo "id=$id" >>"$GITHUB_OUTPUT"
fi
