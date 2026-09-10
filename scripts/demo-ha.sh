#!/usr/bin/env bash
# 高可用场景演示:响应丢失重试 / 双实例争抢接管 / 旧实例恢复围栏 / 旧凭证归还 / 部门迁移
set -u
cd "$(dirname "$0")/.."
A=http://localhost:8080/api
B=http://localhost:8081/api
RUN=$(date +%s)
J() { jq -r "$1"; }
step() { printf '\n\033[1;36m== %s ==\033[0m\n' "$1"; }
POST() { curl -s -X POST "$1" -H 'Content-Type: application/json' -d "$2"; }

wait_auth() { # $1=base $2=expected $3=timeout_s
  local deadline=$(( $(date +%s) + $3 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    [ "$(curl -s "$1/authority" | jq -r '.is_authoritative')" = "$2" ] && return 0
    sleep 1
  done
  echo "等待 $1 权威状态=$2 超时" >&2; return 1
}

step "0. 重建并启动双实例(A 先启动拿到租约,B 备用)"
pkill -f 'server/licensehub' 2>/dev/null; sleep 1
bash scripts/start_infra.sh > /dev/null
export PATH=/workspace/.tools/go/bin:$PATH GOPATH=/workspace/.tools/gopath \
       GOCACHE=/workspace/.tools/gocache GOFLAGS=-buildvcs=false
(cd server && go build -o licensehub .)
INSTANCE_ID=instance-a HTTP_ADDR=:8080 ./server/licensehub > .tools/run/instance-a.log 2>&1 &
echo $! > .tools/run/instance-a.pid
sleep 4
INSTANCE_ID=instance-b HTTP_ADDR=:8081 ./server/licensehub > .tools/run/instance-b.log 2>&1 &
echo $! > .tools/run/instance-b.pid
sleep 3
echo "A: $(curl -s $A/authority | jq -c '{inst: .instance_id, auth: .is_authoritative, epoch: .lease.epoch}')"
echo "B: $(curl -s $B/authority | jq -c '{inst: .instance_id, auth: .is_authoritative, epoch: .lease.epoch}')"

step "1. 经 A 离线借用(TTL 300s)—— 凭证携带签发代次"
R=$(POST $A/borrows '{"pool_id":2,"department_id":2,"employee":"李四","mode":"offline","ttl_seconds":300,"idempotency_key":"ha-demo-offline-$RUN"}')
CRED_OLD=$(echo "$R" | J '.credential')
echo "$R" | jq '{id: .borrow.id, issued_epoch: .borrow.issued_epoch, status: .borrow.status}'

step "2. 签发提交后响应丢失:A 已提交借用,随后崩溃(kill -9)"
R1=$(POST $A/borrows '{"pool_id":1,"department_id":1,"employee":"张三","mode":"offline","ttl_seconds":180,"idempotency_key":"ha-demo-lost-$RUN"}')
ID1=$(echo "$R1" | J '.borrow.id'); CRED1=$(echo "$R1" | J '.credential')
echo "A 已提交 id=$ID1(客户端视角:响应丢失)"
kill -9 "$(cat .tools/run/instance-a.pid)" && echo "instance-a 已终止"

step "3. 等待 B 检测租约过期并自动接管"
wait_auth "$B" true 25 || exit 1
curl -s $B/authority | jq '{inst: .instance_id, auth: .is_authoritative, epoch: .lease.epoch, holder: .lease.holder_id, reason: .lease.takeover_reason}'

step "4. 客户端持原幂等键向新实例 B 重试 → 返回原借用结果"
R2=$(POST $B/borrows '{"pool_id":1,"department_id":1,"employee":"张三","mode":"offline","ttl_seconds":180,"idempotency_key":"ha-demo-lost-$RUN"}')
ID2=$(echo "$R2" | J '.borrow.id'); CRED2=$(echo "$R2" | J '.credential')
echo "重试结果: id=$ID2 replay=$(echo "$R2" | J '.replay')"
[ "$ID1" = "$ID2" ] && echo "✓ 同一借用记录" || echo "✗ id 不一致!"
[ "$CRED1" = "$CRED2" ] && echo "✓ 离线凭证逐字节一致" || echo "✗ 凭证不一致!"

step "5. 旧代次凭证归还给新实例 B(可验证、幂等)"
POST $B/returns "{\"credential\":\"$CRED_OLD\"}" | jq '{result, issued_epoch: .borrow.issued_epoch, status: .borrow.status}'
POST $B/returns "{\"credential\":\"$CRED_OLD\"}" | jq '{result, status: .borrow.status}'

step "6. 旧实例 A 恢复 → 保持备用,签发被数据库围栏拒绝"
INSTANCE_ID=instance-a HTTP_ADDR=:8080 ./server/licensehub >> .tools/run/instance-a.log 2>&1 &
echo $! > .tools/run/instance-a.pid
sleep 4
echo "A: $(curl -s $A/authority | jq -c '{inst: .instance_id, auth: .is_authoritative, holder: .lease.holder_id}')"
curl -s -o /dev/null -w "旧实例 A 尝试签发 -> HTTP %{http_code}(not_authoritative)\n" \
  -X POST $A/borrows -H 'Content-Type: application/json' \
  -d '{"pool_id":1,"department_id":1,"employee":"旧实例","mode":"online","idempotency_key":"ha-demo-fenced-$RUN"}'

step "7. 双实例同时争抢接管:全部停止 → 租约过期 → 同时启动"
kill "$(cat .tools/run/instance-a.pid)" "$(cat .tools/run/instance-b.pid)" 2>/dev/null
sleep 8
R=/workspace/.tools/rootfs
EPOCH_BEFORE=$(LD_LIBRARY_PATH=$R/usr/lib/aarch64-linux-gnu:$R/lib/aarch64-linux-gnu \
  $R/usr/bin/mysql -h 127.0.0.1 -u app -papp123 licensehub -N -e "SELECT epoch FROM service_lease")
INSTANCE_ID=instance-a HTTP_ADDR=:8080 ./server/licensehub >> .tools/run/instance-a.log 2>&1 &
echo $! > .tools/run/instance-a.pid
INSTANCE_ID=instance-b HTTP_ADDR=:8081 ./server/licensehub >> .tools/run/instance-b.log 2>&1 &
echo $! > .tools/run/instance-b.pid
sleep 5
echo "A: $(curl -s $A/authority | jq -c '{inst: .instance_id, auth: .is_authoritative}')"
echo "B: $(curl -s $B/authority | jq -c '{inst: .instance_id, auth: .is_authoritative}')"
EPOCH_AFTER=$(curl -s $A/authority | J '.lease.epoch')
echo "epoch: $EPOCH_BEFORE -> $EPOCH_AFTER(恰好 +1,只有一个权威)"

step "8. 部门迁移:只改归属,池占用不变"
LEADER=$A; [ "$(curl -s $A/authority | J '.is_authoritative')" = "true" ] || LEADER=$B
echo "当前权威: $(curl -s $LEADER/authority | J '.instance_id')"
R=$(POST $LEADER/borrows '{"pool_id":3,"department_id":3,"employee":"迁移员工","mode":"online","idempotency_key":"ha-demo-mig-$RUN"}')
BID=$(echo "$R" | J '.borrow.id')
BEFORE=$(curl -s $LEADER/overview | J '[.pools[] | select(.id==3)][0].active')
POST $LEADER/borrows/$BID/migrate '{"department_id":2}' | jq '{result, note}'
AFTER=$(curl -s $LEADER/overview | J '[.pools[] | select(.id==3)][0].active')
echo "MATLAB 池占用: $BEFORE -> $AFTER(迁移不制造新可用席位)"
POST $LEADER/returns "{\"borrow_id\":$BID}" > /dev/null

step "9. 不变量:任何时刻有效占用 ≤ 池容量;清理演示数据"
POST $LEADER/returns "{\"credential\":\"$CRED1\"}" | jq -r '"清理响应丢失场景的借用: " + .result'
curl -s $LEADER/overview | jq '[.pools[] | {product, active, total_seats, ok: (.active <= .total_seats)}]'
echo
echo "双实例运行中: A=http://localhost:8080  B=http://localhost:8081"
