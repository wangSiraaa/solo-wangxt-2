# LicenseHub · 浮动许可证管理

工程软件浮动许可证的离线借出管理演示:React 展示许可证池与占用来源,Go Gin 提供借还接口,
MySQL 用**事务 + 唯一约束**保证席位正确性,Redis **仅**作查询缓存。
支持**主备双实例高可用**:主实例故障后备用实例自动接管,签发权威由数据库租约 + 单调递增代次裁决。

## 快速开始

```bash
./run.sh                    # 单实例:基础设施 + 服务(http://localhost:8080)
bash scripts/run-ha.sh      # 双实例 HA 模式(instance-a:8080 + instance-b:8081)
bash scripts/demo.sh        # 经典场景演示(自动指向当前权威实例)
bash scripts/demo-ha.sh     # 高可用场景演示(故障转移全流程)
cd server && go test -race -v ./...   # 23 个测试(含 HA 场景)
```

## 架构

```
web/      React + Vite 仪表盘(构建后由 Gin 托管;权威面板展示双实例状态)
server/   Go Gin API(无状态,实例身份由 INSTANCE_ID 区分)
  store.go       事务化席位控制 + 租约协议(AcquireLease/RenewLease)+ 签发围栏
  credential.go  本地模拟签名离线凭证(HMAC-SHA256,携带借用身份与签发代次)
  cache.go       Redis cache-aside,仅服务 GET /api/overview
  http.go        REST 接口
  schema.sql     全量 schema;  migrate_v2.sql 存量库幂等迁移
scripts/  start_infra.sh / run-ha.sh / demo.sh / demo-ha.sh
```

## 高可用设计:权威在数据库,不在 Redis

```
service_lease(单行): epoch 单调递增 | holder_id | lease_expires_at | takeover_reason
```

- **接管**:单条原子 `UPDATE ... WHERE lease_expires_at < now`。并发争抢时行锁序列化,
  后到事务重读最新已提交状态发现租约已有效 → 恰好一个获胜者,epoch 恰好 +1。
- **续租**:`UPDATE ... WHERE holder_id=? AND epoch=?` 双重匹配;被接管后旧实例续租失败即下台。
- **签发围栏**:借用事务内 `SELECT ... FOR UPDATE` 租约行,确认 `holder=自己 且 未过期`,
  把当前 epoch 写入 `borrows.issued_epoch`。检查与签发同事务,不存在权威漂移窗口。
  旧实例恢复后即使本地仍自认权威,签发也会被围栏拒绝(409 not_authoritative)。
- **离线借用不随代次释放**:接管只动租约行;`borrows` 记录原样保留,按原有效期计占用。
- **旧凭证归还**:凭证 HMAC 密钥为服务级共享,新实例可验证旧代次凭证;
  归还不围栏(幂等状态迁移,任何实例执行都安全)。
- **响应丢失跨实例重试**:幂等键唯一约束在共享库中,提交后响应丢失时,
  客户端持原键向新实例重试 → 返回原记录;离线凭证由 `(borrow_id, nonce, exp, epoch)`
  确定性重建,逐字节一致。

## 关键正确性设计

| 需求 | 机制 |
|---|---|
| 最后一席并发只一人成功 | 事务内按 `租约行 → 池行 → 额度行` 固定顺序 `FOR UPDATE`;占用统计用锁定读 |
| 网络重试返回原借用结果 | `idempotency_key` 唯一约束;预检 + 1062 回滚重查;凭证确定性重建 |
| 离线席位到期前不得无凭证释放 | 归还强制校验签名凭证 + nonce 绑定;无凭证 403、伪造 401、过期 410 |
| 过期凭证重复归还 | 状态机只承认 `active → *` 迁移一次;重复归还 `200 already_closed` |
| 缩减额度不抹掉现存借用 | 调额仅写额度行;`count(active) >= quota` 只限制新申请 |
| 主故障备用接管 | 数据库租约过期 + 原子接管;后台循环续租/接管(2s 周期,6s 租约) |
| 旧实例恢复不得签发 | 事务内围栏检查(权威判据只在 MySQL,Redis 不参与) |
| 部门迁移不制造新席位 | 单条 `UPDATE borrows SET department_id` 原子改归属,池级占用不变 |
| Redis 仅缓存查询 | 只有 `GET /api/overview` 走缓存;写路径不碰 Redis,宕机静默降级 |

## API 摘要

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/overview` | 池/部门额度/占用总览(`X-Cache: HIT/MISS`) |
| GET | `/api/authority` | 实例权威状态:租约、代次、本地认知、接管历史 |
| POST | `/api/borrows` | 签发(需权威);`201` 新借,`200+replay` 重放,`409` 额度/围栏 |
| POST | `/api/borrows/:id/migrate` | 部门迁移(需权威,原子改归属) |
| POST | `/api/returns` | 归还(不围栏);在线凭 `borrow_id`,离线凭 `credential`(旧代次可验) |
| POST | `/api/quotas` | 部门负责人调额 |
| POST | `/api/admin/reclaim` | 立即过期回收(另有 5s 后台任务) |
| POST | `/api/admin/takeover` | 手动接管(仅租约过期时生效) |

## 测试(23 个,`go test -race`)

经典:并发最后一席、并发同幂等键、离线提前归还、无凭证/伪造凭证拒绝、
**过期凭证重复归还**、回收只收到期、缩减额度保留现存、缓存 MISS→HIT→失效。

高可用(`ha_test.go`):**8 路并发争抢接管恰好一个获胜且 epoch +1**、非权威签发被围栏、
**旧实例恢复不得签发**(含续租失败)、**离线借用跨接管仍占额度**、
**旧代次凭证在新实例归还且幂等**、**响应丢失跨实例重试返回原结果(凭证逐字节一致)**、
部门迁移不改池占用/无额度部门不产新席位/已关闭不可迁移/迁移需权威、
**接管争抢全程有效占用 ≤ 池容量**。

## 环境变量

`HTTP_ADDR`、`INSTANCE_ID`(默认 instance-a)、`DB_DSN`、`REDIS_ADDR`(置空禁用缓存)、
`CREDENTIAL_SECRET`(各实例共享)、`LEASE_MS`(默认 6000)、`AUTO_ACQUIRE`(默认 1)、`WEB_DIST`。
