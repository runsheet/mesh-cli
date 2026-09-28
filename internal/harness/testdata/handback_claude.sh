#!/bin/sh
# A fake `claude -p` for the hand-back (ADR 012): every invocation's
# arguments and prompt are appended to $HB_RECORD. A fresh session
# writes $HB_TURNS assistant turns of $HB_INPUT uncached input tokens
# each, a Bash call with a 4 KB result in each, then sleeps $HB_SLEEP
# seconds and ends with a result; a resumed one (--resume) answers the
# hand-back turn at once.
prompt=$(cat)
{
  echo "ARGS: $*"
  echo "PROMPT: $prompt"
} >>"$HB_RECORD"
case " $* " in
*" --resume "*)
  echo '{"type":"system","subtype":"init","session_id":"s-hb","model":"claude-sonnet-5"}'
  echo '{"type":"assistant","session_id":"s-hb","message":{"id":"msg_r","role":"assistant","content":[{"type":"text","text":"committed the parser; the CLI flag is not done"}],"usage":{"input_tokens":1000,"output_tokens":100}}}'
  echo '{"type":"result","subtype":"success","session_id":"s-hb","result":"committed what works","total_cost_usd":0.3,"num_turns":1,"usage":{"input_tokens":1000,"output_tokens":100}}'
  exit 0
  ;;
esac
echo '{"type":"system","subtype":"init","session_id":"s-hb","model":"claude-sonnet-5"}'
big=$(head -c 4096 /dev/zero | tr '\0' 'x')
i=1
while [ "$i" -le "${HB_TURNS:-3}" ]; do
  echo "{\"type\":\"assistant\",\"session_id\":\"s-hb\",\"message\":{\"id\":\"msg_$i\",\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_$i\",\"name\":\"Bash\"}],\"usage\":{\"input_tokens\":${HB_INPUT:-500000},\"output_tokens\":0}}}"
  echo "{\"type\":\"user\",\"session_id\":\"s-hb\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"toolu_$i\",\"content\":\"$big\"}]}}"
  i=$((i + 1))
done
sleep "${HB_SLEEP:-0.2}"
echo '{"type":"result","subtype":"success","session_id":"s-hb","result":"all done","total_cost_usd":3.1,"num_turns":3}'
