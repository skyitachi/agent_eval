#!/bin/sh
# A scripted stand-in for a session that called two tools and one of them
# failed.
#
# Every other fixture emits no tool events, so `tool_calls` and `tool_errors`
# are zero across the suite -- and the Tools column of the report's task table
# is the one cell drawn as a count plus a badge. That cell is exactly where a
# raw() fragment concatenated onto a plain string stops being markup and starts
# being printed as its own source, so a fixture that never called a tool leaves
# the column reading "-" and proves nothing about the cell that broke.
#
# The stream is the shape a real CLI emits, trimmed to the events the harness
# parses: an init, two assistant messages, the tool results they produced, and a
# terminal result.
cat > /dev/null

sid="33333333-4444-5555-8666-777777777777"
cat <<JSON
{"type":"system","subtype":"init","session_id":"$sid","tools":["Bash","Read"],"mcp_servers":[],"model":"stand-in","slash_commands":[],"skills":[],"claude_code_version":"0.0.0"}
{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"stand-in","type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","id":"tu-2","name":"Read","input":{"file_path":"missing.txt"}}],"usage":{"input_tokens":120,"output_tokens":30,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"session_id":"$sid"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","is_error":false,"content":"calc.py  logic.py  slugify.py"},{"type":"tool_result","tool_use_id":"tu-2","is_error":true,"content":"Read: missing.txt: No such file or directory"}]},"session_id":"$sid"}
{"type":"assistant","message":{"id":"msg-2","role":"assistant","model":"stand-in","type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"done, and one file was missing"}],"usage":{"input_tokens":140,"output_tokens":25,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"session_id":"$sid"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":2400,"duration_api_ms":2000,"num_turns":2,"result":"done, and one file was missing","session_id":"$sid","total_cost_usd":0.0042,"usage":{"input_tokens":260,"output_tokens":55,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"modelUsage":{"stand-in":{"input_tokens":260,"output_tokens":55}}}
JSON
