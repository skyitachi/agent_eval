import pytest

from calc import average


def test_average_basic():
    assert average([1, 2, 3]) == 2


def test_average_single_element():
    assert average([5]) == 5


def test_average_empty_returns_zero():
    assert average([]) == 0.0


def test_average_floats():
    assert average([1.5, 2.5]) == pytest.approx(2.0)


def test_average_negatives():
    assert average([-2, -4, -6]) == -4
