#!/usr/bin/env bash
# 一键启动:基础设施 -> 后端(含前端静态资源)
set -e
cd "$(dirname "$0")"
bash scripts/start_infra.sh

export PATH=/workspace/.tools/go/bin:$PATH
export GOPATH=/workspace/.tools/gopath GOCACHE=/workspace/.tools/gocache GOFLAGS=-buildvcs=false

if [ ! -f server/licensehub ] || [ server/*.go -nt server/licensehub ]; then
  (cd server && go build -o licensehub .)
fi
if [ ! -d web/dist ]; then
  (cd web && npm install --no-audit --no-fund && npm run build)
fi

echo "访问 http://localhost:8080"
pkill -f 'server/licensehub' 2>/dev/null || true
sleep 1
exec ./server/licensehub
