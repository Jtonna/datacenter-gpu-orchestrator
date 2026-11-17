#!/bin/bash
# lt.sh NAME MODEL WARM(0/1)   - cold load, snapshot, 3 gen requests, optional unload+warm reload+3 gen
NAME=$1; M=$2; WARM=${3:-0}; L=$HOME/llm-scheduler-test/logs; cd $HOME/llm-scheduler-test
S=http://127.0.0.1:3199
P="Write a short paragraph about the history of the bicycle."
echo "== $NAME start $(date -u +%T)" >> $L/$NAME.txt
nvidia-smi --query-gpu=memory.used --format=csv,noheader | sed 's/^/gpu_before /' >> $L/$NAME.txt
./fastpoll.sh $L/$NAME-timeline.jsonl 3000 & FP=$!
python3 chat.py "$M" 60 "$P" 2400 >> $L/$NAME-chat.jsonl
curl -s $S/api/status > $L/status-$NAME-cold.json
echo "gpu_loaded $(nvidia-smi --query-gpu=memory.used --format=csv,noheader)" >> $L/$NAME.txt
nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader >> $L/$NAME.txt
for i in 1 2 3; do python3 chat.py "$M" 200 "$P" 600 >> $L/$NAME-chat.jsonl; done
if [ "$WARM" = 1 ]; then
  curl -s -X POST $S/api/models/unload/$M >> $L/$NAME.txt; echo >> $L/$NAME.txt
  for i in $(seq 60); do n=$(curl -s $S/running | jq '.running|length'); [ "$n" = 0 ] && break; sleep 1; done
  echo "unloaded $(date -u +%T)" >> $L/$NAME.txt
  sleep 5
  python3 chat.py "$M" 60 "$P" 2400 >> $L/$NAME-warm-chat.jsonl
  curl -s $S/api/status > $L/status-$NAME-warm.json
  for i in 1 2 3; do python3 chat.py "$M" 200 "$P" 600 >> $L/$NAME-warm-chat.jsonl; done
fi
curl -s -X POST $S/api/models/unload/$M >> $L/$NAME.txt; echo >> $L/$NAME.txt
for i in $(seq 60); do n=$(curl -s $S/running | jq '.running|length'); [ "$n" = 0 ] && break; sleep 1; done
sleep 3
kill $FP 2>/dev/null
echo "== $NAME done $(date -u +%T) gpu $(nvidia-smi --query-gpu=memory.used --format=csv,noheader)" >> $L/$NAME.txt
