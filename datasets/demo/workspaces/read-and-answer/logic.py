"""Scoring logic used by the ``read-and-answer`` demo task."""


def compute(n):
    total = 0
    for i in range(n):
        total += i * i
    if total > 100:
        return total - 100
    return total + 1


if __name__ == "__main__":
    print(compute(5))
