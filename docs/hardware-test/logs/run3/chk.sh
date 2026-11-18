#!/bin/sh
# chk.sh LABEL : record PS5 payload_version and rpc_port before a test
echo "$1 $(date -u +%T) $(curl -s -m 3 localhost:8090/api/fleet | jq -c '[.members[]|select(.role=="ps5-linux")|{h:.hostname,rpc:.rpc_port,pv:.payload_version}]')" >> $HOME/llm-scheduler-test/logs/ps5-checks.txt
