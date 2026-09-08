#!/usr/bin/env python3
"""Unwrap hard-wrapped markdown into one line per bullet/paragraph.

Usage: tools/unwrap-md.py < section.md > body.md

Used when turning a CHANGELOG.md section into a GitHub release body; see
the "Releasing" section of CLAUDE.md.

CHANGELOG.md is wrapped to ~80 cols to match the repo's other docs, but a
GitHub release body is free text and the hard breaks read badly there.
Structure (headings, bullets, blank lines, indented sub-paragraphs) is
preserved; only the wrapping inside a bullet or paragraph is removed.
"""
import re


def unwrap(text: str) -> str:
    out = []
    # Blocks are runs of consecutive non-blank lines.
    for block in re.split(r'\n\s*\n', text.strip('\n')):
        lines = [l.rstrip() for l in block.split('\n') if l.strip()]
        if not lines:
            continue
        if lines[0].lstrip().startswith('#'):
            out.extend(lines)          # headings stand alone
            continue
        if re.match(r'\s*[-*]\s', lines[0]):
            items = []
            for line in lines:
                if re.match(r'\s*[-*]\s', line):
                    items.append(line.rstrip())
                else:
                    items[-1] += ' ' + line.strip()
            out.extend(items)
            continue
        # Plain paragraph: keep any leading indent, join the rest.
        indent = re.match(r'\s*', lines[0]).group(0)
        out.append(indent + ' '.join(l.strip() for l in lines))
    return '\n\n'.join(
        # Bullets in the same list stay adjacent; everything else gets a
        # blank line between blocks.
        _regroup(out)
    ) + '\n'


def _regroup(lines):
    group, groups = [], []
    for line in lines:
        is_bullet = bool(re.match(r'\s*[-*]\s', line))
        if group and is_bullet and re.match(r'\s*[-*]\s', group[-1]):
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
