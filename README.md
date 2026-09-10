# LicenseHub · 浮动许可证管理

工程软件浮动许可证的离线借出管理演示:React 展示许可证池与占用来源,Go Gin 提供借还接口,
MySQL 用**事务 + 唯一约束**保证席位正确性,Redis **仅**作查询缓存。

## 快速开始

```bash
./run.sh                 # 启动 MariaDB+Redis(本地免root),初始化库表,启动服务
# 打开 http://localhost:8080
bash scripts/demo.sh     # 命令行场景演示(需服务已启动)
cd server && go test ./...   # 运行测试(含并发与过期凭证重复归还)
```

## 架构

```
web/      React + Vite 仪表盘(构建后由 Gin 托管)
server/   Go Gin API
  store.go       事务化席位控制(FOR UPDATE 序列化 + 唯一约束)
  credential.go  本地模拟签名离线凭证(HMAC-SHA256)
  cache.go       Redis cache-aside,仅服务 GET /api/overview
  http.go        REST 接口
scripts/  基础设施与演示脚本
```

## 关键正确性设计

| 需求 | 机制 |
|---|---|
| 最后一席并发只一人成功 | 事务内按 `池行 → 额度行` 顺序 `SELECT ... FOR UPDATE` 序列化,占用统计用锁定读(不受 RR 快照影响),后到事务看到 `count >= quota` 返回 409 |
| 网络重试返回原借用结果 | `borrows.idempotency_key` 唯一约束;先预检,插入撞 1062 则回滚重查原记录;离线凭证由 `(borrow_id, nonce, exp)` 确定性签名,重放重建出同一凭证 |
| 离线席位到期前不得无凭证释放 | 归还接口对离线模式强制校验 HMAC 签名凭证 + nonce 绑定;无凭证 403、伪造 401 |
| 过期凭证重复归还 | 状态机只允许 `active → returned/reclaimed` 迁移一次(`UPDATE ... WHERE status='active'`);已关闭记录返回 `200 already_closed`,席位不会重复释放 |
| 缩减额度不抹掉现存借用 | 调额仅写 `department_quotas`;借用判定 `count(active) >= quota` 自然限制新申请,现存记录不动 |
| Redis 仅缓存查询 | 只有 `GET /api/overview` 走 cache-aside(30s TTL + 写后失效);借用/归还路径不碰 Redis,Redis 宕机静默降级直查 MySQL |

## API 摘要

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/overview` | 池/部门额度/占用总览(`X-Cache: HIT/MISS`) |
| POST | `/api/borrows` | 借用;`201` 新借,`200+replay` 幂等重放,`409` 额度/席位不足 |
| POST | `/api/returns` | 归还;在线凭 `borrow_id`,离线凭 `credential`;`410` 凭证已过期 |
| POST | `/api/quotas` | 部门负责人调额(upsert) |
| POST | `/api/admin/reclaim` | 立即过期回收(另有 5s 后台任务) |

## 测试(`server/server_test.go`,14 个用例)

并发最后一席、并发同幂等键、幂等重放、池总席位上限、离线提前归还、
无凭证/伪造凭证拒绝、**过期凭证重复归还**、回收只收到期、缩减额度保留现存借用、
额度归零、无额度行拒绝、缓存 MISS→HIT→写后失效。

```bash
cd server && go test -race -v ./...
```

## 环境变量

`HTTP_ADDR`(默认 `:8080`)、`DB_DSN`、`REDIS_ADDR`(置空禁用缓存)、
`CREDENTIAL_SECRET`(离线凭证签名密钥,演示默认值仅限本地)、`WEB_DIST`。
