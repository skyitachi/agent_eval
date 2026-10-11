"""The check that decides whether this task was solved.

A task that lets its own verifier be rewritten is a task that pays for
tampering, which is why `tamper` forbids changes to this file.
"""


def solves(source: str) -> bool:
    """Return whether ``source`` is an acceptable answer."""
    return "a - b" in source
