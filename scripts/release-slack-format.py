#!/usr/bin/env python3
"""Build a Slack chat.postMessage payload from a Spire release-notes file.

Reads env: VERSION, NOTES, CHANNEL, REPO. Writes JSON to stdout.

Splits the notes by `## Section` headings into one Slack section block
per heading, rewrites `**bold**` to Slack `*bold*` mrkdwn, and adds a
header block plus a context footer linking to the GitHub release.
"""

from __future__ import annotations

import json
import os
import re
import sys

SECTION_LIMIT = 2900  # Slack section block text cap is 3000 chars; leave a little slack.


def to_mrkdwn(md: str) -> str:
    md = re.sub(r"\*\*(.+?)\*\*", r"*\1*", md, flags=re.DOTALL)
    md = re.sub(r"\[([^\]]+)\]\(([^)]+)\)", r"<\2|\1>", md)
    md = re.sub(r"^###\s+(.*)$", r"*\1*", md, flags=re.MULTILINE)
    md = re.sub(r"^##\s+(.*)$", r"*\1*", md, flags=re.MULTILINE)
    return md


def split_sections(body: str) -> list[tuple[str, str]]:
    sections: list[tuple[str, str]] = []
    cur_title = "Summary"
    cur_buf: list[str] = []
    for line in body.splitlines():
        m = re.match(r"^##\s+(.*)$", line)
        if m:
            if cur_buf:
                sections.append((cur_title, "\n".join(cur_buf).strip()))
            cur_title = m.group(1).strip()
            cur_buf = []
        else:
            cur_buf.append(line)
    if cur_buf:
        sections.append((cur_title, "\n".join(cur_buf).strip()))
    return [(t, b) for t, b in sections if b]


def chunk_section(title: str, body: str) -> list[str]:
    """Split a section into block-sized chunks at bullet boundaries.

    Each chunk is <= SECTION_LIMIT chars; first chunk gets the *title*
    header, continuation chunks are bare so the section reads as one.
    """
    converted = to_mrkdwn(body)
    head = f"*{title}*\n"
    if len(head) + len(converted) <= SECTION_LIMIT:
        return [head + converted]

    # Split body into bullet groups (a "- " bullet plus its indented
    # continuation lines) so we never break mid-bullet.
    groups: list[str] = []
    cur: list[str] = []
    for line in converted.splitlines():
        if line.startswith("- ") and cur:
            groups.append("\n".join(cur))
            cur = [line]
        else:
            cur.append(line)
    if cur:
        groups.append("\n".join(cur))

    chunks: list[str] = []
    buf = head
    for group in groups:
        candidate = buf + ("\n" if buf and not buf.endswith("\n") else "") + group
        if len(candidate) <= SECTION_LIMIT:
            buf = candidate
            continue
        if buf.strip():
            chunks.append(buf.rstrip())
        # Start a new chunk; long single bullets get hard-truncated.
        if len(group) > SECTION_LIMIT:
            chunks.append(group[: SECTION_LIMIT - 2].rstrip() + "\n…")
            buf = ""
        else:
            buf = group
    if buf.strip():
        chunks.append(buf.rstrip())
    return chunks


def main() -> int:
    version = os.environ["VERSION"]
    notes_path = os.environ["NOTES"]
    channel = os.environ["CHANNEL"]
    repo = os.environ["REPO"]

    with open(notes_path, "r", encoding="utf-8") as f:
        raw = f.read()

    lines = raw.splitlines()
    if lines and lines[0].startswith("# "):
        lines = lines[1:]
    body = "\n".join(lines).strip()

    sections = split_sections(body)

    blocks: list[dict] = [
        {
            "type": "header",
            "text": {
                "type": "plain_text",
                "text": f":rocket: Spire {version} released",
                "emoji": True,
            },
        }
    ]

    if sections and sections[0][0] == "Summary":
        summary_text = to_mrkdwn(sections[0][1])
        if len(summary_text) > SECTION_LIMIT:
            summary_text = summary_text[: SECTION_LIMIT - 2].rstrip() + "\n…"
        blocks.append({"type": "section", "text": {"type": "mrkdwn", "text": summary_text}})
        sections = sections[1:]

    if sections:
        blocks.append({"type": "divider"})

    for title, body_ in sections:
        for chunk in chunk_section(title, body_):
            blocks.append({"type": "section", "text": {"type": "mrkdwn", "text": chunk}})

    release_url = f"https://github.com/{repo}/releases/tag/{version}"
    blocks.append(
        {
            "type": "context",
            "elements": [
                {"type": "mrkdwn", "text": f":github: <{release_url}|Full release notes on GitHub>"},
            ],
        }
    )

    payload = {
        "channel": channel,
        "text": f"Spire {version} released",
        "blocks": blocks,
        "unfurl_links": False,
        "unfurl_media": False,
    }
    json.dump(payload, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
