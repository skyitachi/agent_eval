# Task: implement `slugify`

Implement the `slugify(text: str) -> str` function in `slugify.py`.

The stub file and its docstring are already in your working directory.

## Specification

1. Lowercase the input.
2. Replace every run of characters that are **not** ASCII letters or digits with
   a single hyphen (`-`).
3. Strip leading and trailing hyphens.
4. Return the result. An input with no letters or digits yields `""`.

## Examples

| input | output |
| --- | --- |
| `"Hello World"` | `"hello-world"` |
| `"  Mixed   CASE  "` | `"mixed-case"` |
| `"a--b"` | `"a-b"` |
| `"Hello, World!"` | `"hello-world"` |
| `"already-slugged"` | `"already-slugged"` |
| `"123 ABC"` | `"123-abc"` |
| `"!!!"` | `""` |

## Rules

- Standard library only.
- Only edit `slugify.py`.
- Your implementation will be checked against a test suite you cannot see.
  Make it satisfy the specification exactly, not just the examples shown.

When you are done, reply with a one-line summary of the change.
