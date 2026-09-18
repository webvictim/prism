#!/usr/bin/env python3
"""Tests for tools/unwrap-md.py.

Run: python3 tools/unwrap_md_test.py

The script's filename is hyphenated so it reads well as a command, which
makes it un-importable by name; load it by path instead.
"""
import importlib.util
import pathlib
import re
import unittest

_HERE = pathlib.Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location('unwrap_md', _HERE / 'unwrap-md.py')
unwrap_md = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(unwrap_md)
unwrap = unwrap_md.unwrap


class UnwrapTest(unittest.TestCase):
    def test_wrapped_bullet_is_joined(self):
        self.assertEqual(
            unwrap('- one two\n  three four\n'),
            '- one two three four\n',
        )

    def test_wrapped_paragraph_is_joined(self):
        self.assertEqual(
            unwrap('plain one\ntwo three\n'),
            'plain one two three\n',
        )

    # Regression: a non-bullet paragraph after a bullet list was being
    # appended to the last bullet, which silently swallowed the
    # "**Full Changelog**" trailer into the final list item.
    def test_paragraph_after_bullets_stays_separate(self):
        got = unwrap('- bullet one\n  tail\n- bullet two\n\n**Trailer**: separate\n')
        self.assertEqual(
            got,
            '- bullet one tail\n- bullet two\n\n**Trailer**: separate\n',
        )

    def test_consecutive_bullets_stay_contiguous(self):
        # A list is one block: no blank lines injected between its items.
        self.assertEqual(unwrap('- a\n- b\n- c\n'), '- a\n- b\n- c\n')

    def test_asterisk_bullets(self):
        self.assertEqual(unwrap('* a one\n  a two\n* b\n'), '* a one a two\n* b\n')

    def test_headings_stand_alone(self):
        got = unwrap('### Fixed\n\n- a thing\n  wrapped\n')
        self.assertEqual(got, '### Fixed\n\n- a thing wrapped\n')

    def test_heading_never_absorbs_following_text(self):
        # Even with no blank line after it.
        got = unwrap('### Fixed\n- a thing\n')
        self.assertEqual(got, '### Fixed\n\n- a thing\n')

    def test_indented_subparagraph_keeps_indent_and_separation(self):
        src = ('- a bullet that\n  wraps\n\n'
               '  *an indented note that\n  also wraps*\n')
        self.assertEqual(
            unwrap(src),
            '- a bullet that wraps\n\n  *an indented note that also wraps*\n',
        )

    def test_fence_is_verbatim_including_blank_lines(self):
        src = ('intro line\nwrapped\n\n```bash\nprism down && prism up\n\n'
               'second command\n```\n\n- after\n')
        got = unwrap(src)
        self.assertIn('```bash\nprism down && prism up\n\nsecond command\n```', got)
        self.assertIn('intro line wrapped', got)
        self.assertTrue(got.rstrip().endswith('- after'))

    def test_bullet_inside_fence_is_not_treated_as_a_bullet(self):
        src = '```\n- not a real bullet\n  still inside the fence\n```\n'
        self.assertIn('- not a real bullet\n  still inside the fence', unwrap(src))

    def test_output_ends_with_exactly_one_newline(self):
        for src in ('- a\n', 'para\n', '### H\n\n- a\n', '```\nx\n```\n'):
            got = unwrap(src)
            self.assertTrue(got.endswith('\n'), repr(got))
            self.assertFalse(got.endswith('\n\n'), repr(got))

    def test_idempotent(self):
        src = ('### Added\n\n- one that\n  wraps\n- two\n\n'
               '**Trailer**: here\n\n```sh\ncmd --flag\n```\n')
        once = unwrap(src)
        self.assertEqual(unwrap(once), once)

    def test_empty_input(self):
        self.assertEqual(unwrap(''), '\n')


class ChangelogTest(unittest.TestCase):
    """Unwrapping the real CHANGELOG must not lose or merge structure."""

    def setUp(self):
        self.changelog = (_HERE.parent / 'CHANGELOG.md').read_text()

    def sections(self):
        parts = re.split(r'^## \[([^\]]+)\][^\n]*$', self.changelog, flags=re.M)
        for i in range(1, len(parts), 2):
            body = re.sub(r'\n\[[^\]]+\]: \S+.*$', '', parts[i + 1], flags=re.S)
            yield parts[i], body

    def test_structure_preserved_for_every_release_section(self):
        checked = 0
        for version, body in self.sections():
            got = unwrap(body)
            for pattern in (r'^- ', r'^###'):
                self.assertEqual(
                    len(re.findall(pattern, body, flags=re.M)),
                    len(re.findall(pattern, got, flags=re.M)),
                    f'{version}: {pattern!r} count changed',
                )
            checked += 1
        self.assertGreater(checked, 15, 'expected the full release history')

    def test_no_bullet_swallows_a_following_paragraph(self):
        for version, body in self.sections():
            for line in unwrap(body + '\n**Full Changelog**: x\n').split('\n'):
                if line.startswith('- '):
                    self.assertNotIn('**Full Changelog**', line, f'{version}')


if __name__ == '__main__':
    unittest.main(verbosity=2)
