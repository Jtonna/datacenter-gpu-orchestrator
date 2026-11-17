#!/bin/sh
# bounded status poller: every 3 s for at most 2 h
OUT=$HOME/llm-scheduler-test/logs/status-timeline.jsonl
i=0
while [ $i -lt 2400 ]; do
  curl -s -m 2 http://127.0.0.1:3199/api/status | jq -c '{t:.time, inst:[.instances[]?|{p:.profile,s:.state,inf:.in_flight,n:[.nodes[]?.id]}], q:[.queue[]?|{p:.profile,c:.claimed,claim:.claim,v:.victims,w:.waiters}], quar:.quarantines, bo:.backoffs}' >> $OUT 2>/dev/null
  i=$((i+1)); sleep 3
done
