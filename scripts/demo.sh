#!/usr/bin/env bash
# LicenseHub 场景演示:借出 / 提前归还 / 过期回收 / 幂等重试 / 并发抢最后席位 / 额度调整
set -u
# 未指定 BASE 时自动指向当前权威实例(单实例模式即 8080)
if [ -z "${BASE:-}" ]; then
  for port in 8080 8081; do
    if [ "$(curl -s "http://localhost:$port/api/authority" 2>/dev/null | jq -r '.is_authoritative' 2>/dev/null)" = "true" ]; then
      BASE="http://localhost:$port/api"; break
    fi
  done
  BASE="${BASE:-http://localhost:8080/api}"
fi
echo "目标实例: $BASE"
RUN=$(date +%s)
J() { jq -r "$1"; }
step() { printf '\n\033[1;36m== %s ==\033[0m\n' "$1"; }

step "0. 健康检查与总览(观察 X-Cache: 第一次 MISS,第二次 HIT)"
curl -s -o /dev/null -D - "$BASE/overview" | grep -i x-cache
curl -s -o /dev/null -D - "$BASE/overview" | grep -i x-cache
curl -s "$BASE/overview" | jq '{pools: [.pools[] | {product, total_seats, active}], active_borrows: (.active_borrows|length)}'

step "1. 在线借用 + 直接归还"
R=$(curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  '{"pool_id":1,"department_id":1,"employee":"张三","mode":"online","idempotency_key":"demo-online-$RUN"}')
echo "$R" | jq '{id: .borrow.id, status: .borrow.status, replay}'
BID=$(echo "$R" | J '.borrow.id')
curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"borrow_id\":$BID}" | jq '{result, status: .borrow.status}'

step "2. 离线借用(120s) + 签名凭证提前归还"
R=$(curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  '{"pool_id":2,"department_id":2,"employee":"李四","mode":"offline","ttl_seconds":120,"idempotency_key":"demo-offline-$RUN-a"}')
CRED=$(echo "$R" | J '.credential')
echo "凭证: ${CRED:0:48}…"
curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"credential\":\"$CRED\"}" | jq '{result, status: .borrow.status}'

step "3. 离线席位到期前:无凭证归还被拒绝(403)"
R=$(curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  '{"pool_id":3,"department_id":3,"employee":"王五","mode":"offline","ttl_seconds":2,"idempotency_key":"demo-offline-$RUN-b"}')
BID=$(echo "$R" | J '.borrow.id'); CRED=$(echo "$R" | J '.credential')
curl -s -o /dev/null -w "无凭证归还 -> HTTP %{http_code}\n" -X POST "$BASE/returns" \
  -H 'Content-Type: application/json' -d "{\"borrow_id\":$BID}"

step "4. 等待过期 -> 过期凭证主动归还(410) -> 回收 -> 过期凭证重复归还(already_closed)"
sleep 3
curl -s -o /dev/null -w "过期凭证归还(回收前) -> HTTP %{http_code}\n" -X POST "$BASE/returns" \
  -H 'Content-Type: application/json' -d "{\"credential\":\"$CRED\"}"
curl -s -X POST "$BASE/admin/reclaim" | jq '{reclaimed}'
echo "-- 过期凭证重复归还两次:"
curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"credential\":\"$CRED\"}" | jq '{result, status: .borrow.status}'
curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"credential\":\"$CRED\"}" | jq '{result, status: .borrow.status}'

step "5. 幂等重试:同一幂等键提交两次,返回同一条借用"
K="demo-idem-$(date +%s)"
R1=$(curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  "{\"pool_id\":1,\"department_id\":1,\"employee\":\"赵六\",\"mode\":\"online\",\"idempotency_key\":\"$K\"}")
R2=$(curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  "{\"pool_id\":1,\"department_id\":1,\"employee\":\"赵六\",\"mode\":\"online\",\"idempotency_key\":\"$K\"}")
echo "第一次: id=$(echo "$R1" | J '.borrow.id') replay=$(echo "$R1" | J '.replay')"
echo "第二次: id=$(echo "$R2" | J '.borrow.id') replay=$(echo "$R2" | J '.replay')"
BID=$(echo "$R1" | J '.borrow.id')
curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"borrow_id\":$BID}" > /dev/null

step "6. 并发抢最后一个席位(ANSYS/结构设计部 额度=1,8 个并发)"
PIDS=(); OUT=/tmp/licensehub-race; rm -f $OUT.*
for i in $(seq 1 8); do
  (curl -s -o /dev/null -w "%{http_code}\n" -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
    "{\"pool_id\":2,\"department_id\":1,\"employee\":\"并发$i\",\"mode\":\"online\",\"idempotency_key\":\"demo-race-$RUN-$i\"}" \
    > $OUT.$i) & PIDS+=($!)
done
wait "${PIDS[@]}"
echo "结果分布: $(cat $OUT.* | sort | uniq -c | tr '\n' ' ')  (201=抢到, 409=被拒)"
R=$(curl -s "$BASE/overview")
echo "$R" | jq '.active_borrows[] | select(.pool_id==2 and .department=="结构设计部") | {id, employee}'
BID=$(echo "$R" | J '[.active_borrows[] | select(.pool_id==2 and .department=="结构设计部")][0].id')
curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"borrow_id\":$BID}" > /dev/null
echo "(已归还,恢复现场)"

step "7. 额度调整:AutoCAD/仿真分析部 3 -> 1(先借 2 个,缩减后现存保留、新申请被拒)"
curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  '{"pool_id":1,"department_id":2,"employee":"孙七","mode":"online","idempotency_key":"demo-quota-$RUN-1"}' > /dev/null
curl -s -X POST "$BASE/borrows" -H 'Content-Type: application/json' -d \
  '{"pool_id":1,"department_id":2,"employee":"周八","mode":"online","idempotency_key":"demo-quota-$RUN-2"}' > /dev/null
curl -s -X POST "$BASE/quotas" -H 'Content-Type: application/json' -d \
  '{"pool_id":1,"department_id":2,"quota":1,"adjusted_by":"仿真部负责人"}' | jq '{quota, active_borrows, note}'
curl -s -o /dev/null -w "超额后新申请 -> HTTP %{http_code}\n" -X POST "$BASE/borrows" \
  -H 'Content-Type: application/json' -d \
  '{"pool_id":1,"department_id":2,"employee":"吴九","mode":"online","idempotency_key":"demo-quota-$RUN-3"}'
echo "-- 恢复现场:归还并调回额度"
for k in demo-quota-$RUN-1 demo-quota-$RUN-2; do
  BID=$(curl -s "$BASE/overview" | J "[.active_borrows[] | select(.pool_id==1 and .department==\"仿真分析部\")][0].id")
  [ "$BID" != "null" ] && curl -s -X POST "$BASE/returns" -H 'Content-Type: application/json' -d "{\"borrow_id\":$BID}" > /dev/null
done
curl -s -X POST "$BASE/quotas" -H 'Content-Type: application/json' -d \
  '{"pool_id":1,"department_id":2,"quota":3,"adjusted_by":"仿真部负责人"}' | jq '{quota, active_borrows}'

step "完成。最终总览:"
curl -s "$BASE/overview" | jq '{pools: [.pools[] | {product, active, online, offline}], active_borrows: (.active_borrows|length)}'
