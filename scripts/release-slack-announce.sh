#!/usr/bin/env bash
# Post a Spire release announcement to Slack.
#
# Reads release notes from `releases/<version>.md` (or argument) and
# posts to the configured Slack channel as a chat.postMessage with
# header + section blocks. Each markdown `## Section` becomes its own
# Slack section block, with `**bold**` rewritten to Slack `*bold*`
# mrkdwn.
#
# Usage:
#   scripts/release-slack-announce.sh                # auto-pick latest releases/v*.md
#   scripts/release-slack-announce.sh v0.47.0        # specific version
#   scripts/release-slack-announce.sh path/to/notes.md
#
# Env:
#   SLACK_TOKEN_FILE   token path (default: ~/.config/slack-bot-token,
#                      falls back to ~/.config/spire-bot-token)
#   SLACK_CHANNEL      channel ID (default: C02UCFP75NY)
#   GITHUB_REPO        owner/repo for release link (default: awell-health/spire)
#   DRY_RUN=1          print the payload, do not send

set -euo pipefail

cd "$(dirname "$0")/.."

CHANNEL="${SLACK_CHANNEL:-C02UCFP75NY}"
REPO="${GITHUB_REPO:-awell-health/spire}"
DRY_RUN="${DRY_RUN:-0}"

if [[ -n "${SLACK_TOKEN_FILE:-}" ]]; then
    TOKEN_FILE="$SLACK_TOKEN_FILE"
elif [[ -f "$HOME/.config/slack-bot-token" ]]; then
    TOKEN_FILE="$HOME/.config/slack-bot-token"
elif [[ -f "$HOME/.config/spire-bot-token" ]]; then
    TOKEN_FILE="$HOME/.config/spire-bot-token"
else
    echo "error: no slack token found at ~/.config/slack-bot-token or ~/.config/spire-bot-token" >&2
    exit 2
fi

if [[ ! -r "$TOKEN_FILE" ]]; then
    echo "error: slack token file not readable: $TOKEN_FILE" >&2
    exit 2
fi

TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
if [[ -z "$TOKEN" ]]; then
    echo "error: slack token file is empty: $TOKEN_FILE" >&2
    exit 2
fi

ARG="${1:-}"
if [[ -z "$ARG" ]]; then
    NOTES="$(ls -1 releases/v*.md 2>/dev/null | sort -V | tail -n1 || true)"
    if [[ -z "$NOTES" ]]; then
        echo "error: no releases/v*.md files found" >&2
        exit 2
    fi
elif [[ -f "$ARG" ]]; then
    NOTES="$ARG"
elif [[ -f "releases/${ARG}.md" ]]; then
    NOTES="releases/${ARG}.md"
elif [[ -f "releases/v${ARG}.md" ]]; then
    NOTES="releases/v${ARG}.md"
else
    echo "error: notes file not found for '$ARG'" >&2
    exit 2
fi

VERSION="$(basename "$NOTES" .md)"
if [[ "$VERSION" != v* ]]; then
    VERSION="v${VERSION}"
fi

echo "==> announcing $VERSION from $NOTES to $CHANNEL"

PAYLOAD_FILE="$(mktemp -t spire-slack-payload.XXXXXX)"
trap 'rm -f "$PAYLOAD_FILE"' EXIT

VERSION="$VERSION" NOTES="$NOTES" CHANNEL="$CHANNEL" REPO="$REPO" \
    python3 "$(dirname "$0")/release-slack-format.py" > "$PAYLOAD_FILE"

if [[ "$DRY_RUN" == "1" ]]; then
    python3 -m json.tool < "$PAYLOAD_FILE"
    exit 0
fi

RESP_FILE="$(mktemp -t spire-slack-resp.XXXXXX)"
trap 'rm -f "$PAYLOAD_FILE" "$RESP_FILE"' EXIT

curl -sS -X POST https://slack.com/api/chat.postMessage \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json; charset=utf-8" \
    --data-binary "@$PAYLOAD_FILE" > "$RESP_FILE"

OK="$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print("yes" if d.get("ok") else "no")' "$RESP_FILE")"
if [[ "$OK" != "yes" ]]; then
    echo "error: slack rejected the message" >&2
    python3 -m json.tool < "$RESP_FILE" >&2 || cat "$RESP_FILE" >&2
    exit 1
fi

TS="$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d.get("ts",""))' "$RESP_FILE")"
echo "==> posted: ts=$TS channel=$CHANNEL"
