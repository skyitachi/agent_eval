#!/bin/sh
# A scripted stand-in for a model whose second attempt depends on its first.
#
# The other fixtures answer the same way whatever they were asked and whatever
# is on disk, which is what makes them useless for the chained-replay tests: the
# question there is whether a session's later task can see what its earlier one
# left, and a fixture that ignores its working directory cannot tell the two
# outcomes apart.
#
# WRITE leaves a file in the working directory and says so. READ reports whether
# that file is there and what it says. Run the pair in one workspace and READ
# finds it; run them in separate workspaces and it does not -- which is the
# whole difference between a chained run and an unchained one.
#
# The stream is the shape a real CLI emits, trimmed to the events the harness
# parses: an init event, one assistant message carrying usage, and a terminal
# result.
prompt=$(cat)
case "$prompt" in
  *WRITE*)
    printf 'the-first-turn-wrote-this' > handoff.txt
    answer="wrote handoff.txt"
    ;;
  *READ*)
    if [ -f handoff.txt ]; then
      answer="found: $(cat handoff.txt)"
    else
      answer="missing"
    fi
    ;;
  *)
    answer="nothing to do"
    ;;
esac
sid="22222222-3333-4444-8555-666666666666"
cat <<JSON
{"type":"system","subtype":"init","session_id":"$sid","tools":["Bash","Read"],"mcp_servers":[],"model":"stand-in","slash_commands":[],"skills":[],"claude_code_version":"0.0.0"}
{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"stand-in","type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"$answer"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"session_id":"$sid"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":120,"duration_api_ms":100,"num_turns":1,"result":"$answer","session_id":"$sid","total_cost_usd":0.001,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"modelUsage":{"stand-in":{"input_tokens":100,"output_tokens":20}}}
JSON
