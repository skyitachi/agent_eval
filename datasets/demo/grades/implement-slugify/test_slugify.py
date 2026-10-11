"""Hidden grading suite for the ``implement-slugify`` demo task.

Injected into the sandbox after the agent finishes, so the agent cannot read or
modify it during the attempt.
"""

import pytest

from slugify import slugify


@pytest.mark.parametrize(
    ("raw", "want"),
    [
        ("Hello World", "hello-world"),
        ("  Mixed   CASE  ", "mixed-case"),
        ("a--b", "a-b"),
        ("Hello, World!", "hello-world"),
        ("already-slugged", "already-slugged"),
        ("123 ABC", "123-abc"),
        ("!!!", ""),
        ("", ""),
        ("trailing---", "trailing"),
        ("---leading", "leading"),
        ("tab\tand\nnewline", "tab-and-newline"),
    ],
)
def test_slugify(raw, want):
    assert slugify(raw) == want


def test_slugify_returns_str():
    assert isinstance(slugify("Hello"), str)
