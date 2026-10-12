#!/bin/sh
# A scripted stand-in for a model that reads the prompt and answers in one turn.
#
# The other fixtures are shaped for specific failures -- good.sh solves the demo,
# bad.sh solves it wrongly, slash-command.sh reproduces a stream that never
# reached the model at all. This one exists for the tests whose subject is the
# *configuration* an attempt ran under rather than what it produced: it always
# succeeds, in one turn, so a comparison between two such runs differs in nothing
# but the thing the test changed.
#
# The stream is the shape a real CLI emits, trimmed to the events the harness
# parses: an init event, one assistant message carrying usage, and a terminal
# result. `slash_commands` is empty because that is what the clean-room flags
# leave behind, and `num_turns` is non-zero so the attempt is a measurement
# rather than the "never ran" case the harness reports separately.
cat > /dev/null
sid="11111111-2222-4333-8444-555555555555"
cat <<JSON
{"type":"system","subtype":"init","session_id":"$sid","tools":["Bash","Read"],"mcp_servers":[],"model":"stand-in","slash_commands":[],"skills":[],"claude_code_version":"0.0.0"}
{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"stand-in","type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"session_id":"$sid"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":120,"duration_api_ms":100,"num_turns":1,"result":"done","session_id":"$sid","total_cost_usd":0.001,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"modelUsage":{"stand-in":{"input_tokens":100,"output_tokens":20}}}
JSON
