#!/usr/bin/env bash
# 双实例高可用模式:instance-a(8080)+ instance-b(8081),共享同一 MySQL/Redis
set -e
cd "$(dirname "$0")/.."
bash scripts/start_infra.sh

export PATH=/workspace/.tools/go/bin:$PATH
export GOPATH=/workspace/.tools/gopath GOCACHE=/workspace/.tools/gocache GOFLAGS=-buildvcs=false
(cd server && go build -o licensehub .)
[ -d web/dist ] || (cd web && npm install --no-audit --no-fund && npm run build)

pkill -f 'server/licensehub' 2>/dev/null || true
sleep 1

INSTANCE_ID=instance-a HTTP_ADDR=:8080 ./server/licensehub > .tools/run/instance-a.log 2>&1 &
echo $! > .tools/run/instance-a.pid
INSTANCE_ID=instance-b HTTP_ADDR=:8081 ./server/licensehub > .tools/run/instance-b.log 2>&1 &
echo $! > .tools/run/instance-b.pid

sleep 3
echo "双实例已启动:"
echo "  instance-a  http://localhost:8080  (日志 .tools/run/instance-a.log)"
echo "  instance-b  http://localhost:8081  (日志 .tools/run/instance-b.log)"
curl -s localhost:8080/api/authority | jq '{instance: .instance_id, authoritative: .is_authoritative, epoch: .lease.epoch, holder: .lease.holder_id}'
curl -s localhost:8081/api/authority | jq '{instance: .instance_id, authoritative: .is_authoritative, epoch: .lease.epoch, holder: .lease.holder_id}'
