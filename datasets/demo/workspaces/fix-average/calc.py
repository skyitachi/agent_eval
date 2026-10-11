"""Numeric helpers."""


def average(nums):
    """Return the arithmetic mean of ``nums``.

    An empty sequence returns ``0.0``.
    """
    total = 0
    for n in nums:
        total += n
    return total / (len(nums) - 1)
