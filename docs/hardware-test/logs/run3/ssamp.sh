#!/bin/sh
# every 5 s: tx bytes of the scheduler container's eth0 (the children's RPC uploads dominate)
OUT=$1
while true; do
  echo "$(date -u +%T) $(docker exec llm-scheduler-test cat /proc/net/dev 2>/dev/null | awk '/eth0/{print $2, $10}')" >> $OUT
  sleep 5
done
