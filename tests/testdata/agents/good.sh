#!/bin/sh
# A scripted stand-in for a model that solves every demo task correctly.
#
# The harness runs this with the task prompt on stdin and the workspace as the
# working directory, so the agent can be dispatched on what it finds there.
# Keeping a "known good" agent in the repository is what turns the demo dataset
# into a regression test: if the pipeline starts scoring a correct patch as a
# failure, this is the fixture that notices.
cat > /dev/null

if [ -f calc.py ]; then
  cat > calc.py <<'PY'
"""Numeric helpers."""


def average(nums):
    """Return the arithmetic mean of ``nums``.

    An empty sequence returns ``0.0``.
    """
    if not nums:
        return 0.0
    return sum(nums) / len(nums)
PY
  echo "fixed average(): divide by len(nums) and return 0.0 for an empty sequence"
elif [ -f slugify.py ]; then
  cat > slugify.py <<'PY'
"""Slug generation helpers."""

import re


def slugify(text: str) -> str:
    """Convert ``text`` into a URL-friendly slug."""
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
PY
  echo "implemented slugify(): lowercase, collapse non-alphanumerics, strip hyphens"
elif [ -f logic.py ]; then
  echo 31
fi
