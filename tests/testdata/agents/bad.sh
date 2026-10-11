#!/bin/sh
# A scripted stand-in for a model that fails every demo task.
#
# The failures are chosen to be *interesting* rather than merely empty: each one
# is a plausible wrong answer that a careless agent would produce. "Off by one
# in the denominator", "spaces are the only thing to strip", "answer 30 instead
# of 31" -- a grader that passes any of these is broken, and a report with only
# green rows proves nothing.
cat > /dev/null

if [ -f calc.py ]; then
  cat > calc.py <<'PY'
"""Numeric helpers."""


def average(nums):
    """Return the arithmetic mean of ``nums``."""
    total = 0
    for n in nums:
        total += n
    return total / (len(nums) - 1)
PY
  echo "adjusted average() without changing the denominator"
elif [ -f slugify.py ]; then
  cat > slugify.py <<'PY'
"""Slug generation helpers."""


def slugify(text: str) -> str:
    """Convert ``text`` into a URL-friendly slug."""
    return text.lower().replace(" ", "-")
PY
  echo "implemented slugify() by replacing spaces only"
elif [ -f logic.py ]; then
  echo 30
fi
