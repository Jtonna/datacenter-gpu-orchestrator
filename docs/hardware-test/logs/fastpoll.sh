#!/bin/sh
# fastpoll.sh OUTFILE SECONDS : 1-second status poll
OUT=$1; N=$2; i=0
while [ $i -lt $N ]; do
  curl -s -m 1 http://127.0.0.1:3199/api/status | jq -c '{t:.time[11:22], inst:[.instances[]?|"\(.profile)=\(.state)\(if .phase!="" then "/"+.phase else "" end) inf=\(.in_flight) victim_of=\(.victim_of)"], q:[.queue[]?|{p:.profile,claimed:.claimed,claim:.claim,victims:.victims,w:.waiters}]}' >> $OUT 2>/dev/null
  i=$((i+1)); sleep 1
done
