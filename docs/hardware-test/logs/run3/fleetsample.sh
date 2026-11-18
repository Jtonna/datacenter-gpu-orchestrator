#!/bin/sh
# fleetsample.sh OUT : every 3 s, one line per node with ip, rpc_port, payload, vram_used, ram_used (until killed)
OUT=$1
while true; do
  curl -s -m 2 http://127.0.0.1:8090/api/fleet | jq -c '{t:(now|todate|.[11:19]), n:[.members[]|{h:.hostname[0:12],ip,rpc:.rpc_port,pv:(.payload_version//"-")[0:12],vram:.vram_used,ram:.ram_used_gb}]}' >> $OUT 2>/dev/null
  sleep 3
done
