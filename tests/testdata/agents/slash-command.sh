#!/bin/sh
# Prints the exact stream a real Claude Code emits when a prompt opens with a
# slash command it does not expose.
#
# Captured from Claude Code 2.1.205 under the clean-room flags the harness passes
# by default. Nothing here is a model's answer: the CLI replies locally and exits
# zero, reporting subtype "success" with is_error false, so an attempt that never
# reached the API is indistinguishable from one that answered in zero tokens.
#
# Kept verbatim rather than paraphrased -- the shape of the lie is the point, and
# a hand-written stand-in would only test what we already believed.
cat > /dev/null
dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cat "$dir/slash-command-stream.jsonl"
