#!/usr/bin/env python3
"""Unwrap hard-wrapped markdown into one line per bullet/paragraph.

Usage: tools/unwrap-md.py < section.md > body.md

Used when turning a CHANGELOG.md section into a GitHub release body; see
the "Releasing" section of CLAUDE.md.

CHANGELOG.md is wrapped to ~80 cols to match the repo's other docs, but a
GitHub release body is free text and the hard breaks read badly there.
Structure (headings, bullets, blank lines, indented sub-paragraphs) is
preserved; only the wrapping inside a bullet or paragraph is removed.

Fenced code blocks are passed through verbatim — joining a log line or a
shell snippet onto one line would corrupt it — including any blank lines
inside them.
"""
import re

FENCE_RE = re.compile(r'^\s*```')
BULLET_RE = re.compile(r'^\s*[-*]\s')


def _segments(text):
    """Split text into ('prose'|'fence', lines) runs.

    Fences are separated out before any blank-line splitting, so a code
    block containing a blank line survives intact.
    """
    segments, current, in_fence = [], [], False
    for line in text.split('\n'):
        if FENCE_RE.match(line):
            if in_fence:
                current.append(line)
                segments.append(('fence', current))
                current, in_fence = [], False
            else:
                if current:
                    segments.append(('prose', current))
                current, in_fence = [line], True
            continue
        current.append(line)
    if current:
        segments.append(('fence' if in_fence else 'prose', current))
    return segments


def unwrap(text: str) -> str:
    out = []
    for kind, seg_lines in _segments(text.strip('\n')):
        if kind == 'fence':
            out.append('\n'.join(l.rstrip() for l in seg_lines))
            continue
        # Blocks are runs of consecutive non-blank lines.
        for block in re.split(r'\n\s*\n', '\n'.join(seg_lines)):
            lines = [l.rstrip() for l in block.split('\n') if l.strip()]
            if not lines:
                continue
            if lines[0].lstrip().startswith('#'):
                out.extend(lines)      # headings stand alone
                continue
            # A bullet marker starts a new logical line; anything else
            # continues the current one. A bullet may follow an indented
            # sub-paragraph with no blank line between them, so this is
            # decided per line rather than from the block's first line.
            for line in lines:
                if BULLET_RE.match(line) or not out or _closed(out[-1]):
                    out.append(line)
                else:
                    out[-1] += ' ' + line.strip()
    return '\n\n'.join(_regroup(out)) + '\n'


def _closed(entry):
    """True if entry can't take continuation lines (heading or fence)."""
    return entry.lstrip().startswith('#') or FENCE_RE.match(entry)


def _regroup(lines):
    group, groups = [], []
    for line in lines:
        is_bullet = bool(BULLET_RE.match(line))
        if group and is_bullet and BULLET_RE.match(group[-1]):
            group.append(line)
        else:
            if group:
                groups.append('\n'.join(group))
            group = [line]
    if group:
        groups.append('\n'.join(group))
    return groups


if __name__ == '__main__':
    import sys
    sys.stdout.write(unwrap(sys.stdin.read()))
