#!/bin/sh
# A fake `claude -p` that writes stream-json slowly: an init, then
# SLOW_EVENTS assistant events SLOW_SLEEP seconds apart, then a result.
cat >/dev/null
n=${SLOW_EVENTS:-5}
pause=${SLOW_SLEEP:-0.1}
echo '{"type":"system","subtype":"init","session_id":"s-slow","model":"claude-sonnet-5"}'
i=1
while [ "$i" -le "$n" ]; do
  echo "{\"type\":\"assistant\",\"session_id\":\"s-slow\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"step $i\"}]}}"
  sleep "$pause"
  i=$((i + 1))
done
echo '{"type":"result","subtype":"success","session_id":"s-slow","result":"all done","total_cost_usd":0.42,"num_turns":3}'
