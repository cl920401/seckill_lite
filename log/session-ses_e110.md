# Go+Redis+MySQL 秒杀系统方案设计

**Session ID:** ses_e11076e52ffeZEOZTjP676vi73
**Created:** 11/18/2026, 10:03:50 PM
**Updated:** 11/18/2026, 11:28:20 PM

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: write

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: write

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: write

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

已按规范完成 Go MVP 实现。未执行数据库迁移、MySQL 集成测试或压测；未提交 commit。

## 已实现

- `cmd/api/main.go`：启动装配、Stream group 初始化、Worker、取消任务、健康检查、优雅退出。
- `cmd/loadtest/main.go`：并发压测、重复幂等键模式、可选 MySQL 只读校验。
- `internal/service/service.go`：全局/用户/查询限流、幂等回放、活动校验、Redis 预扣、202 语义。
- `internal/mysqlrepo/store.go`：事务落库、条件扣库存、限购、支付、超时取消、请求/预约终态。
- `internal/worker/worker.go`：Stream 消费、`XAUTOCLAIM`、成功回填、拒绝/额外预约补偿、DONE 直接 ACK。
- `internal/httpapi/api.go`：运营、下单、查询、模拟支付、健康检查。
- `internal/redisrepo` + `internal/lua`：预扣、补偿、回补、状态更新、令牌桶脚本。
- `migrations/0001_init.sql`、`scripts/setup_db.sh`、`scripts/verify.sql`、`Makefile`、`.env.example`。

关键实现点：

- 首次购买先确保 `user_purchases` 以 `qty=0` 建行，再统一执行限购条件更新，避免直接插入购买数量。
- 完整限流顺序：ingress 全局限流 → 幂等查询 → 新单用户限流 → 活动/依赖 → Lua。
- 同一幂等键并发重试可能返回 429；未被限流的请求不会重复建单。
- Worker 仅在 DB 提交后 ACK；拒绝/额外预约在补偿成功或 DB 标记 DONE 后 ACK。
- `sk:restocked` 保持 24h TTL，代码不会因标记过期自动回补。
- 已覆盖 `docs/SPEC.md` 中新增的首次限购、MySQL 故障窗口和补偿 DONE 语义。

## 已验证

```text
gofmt -l .          通过（无输出）
go vet ./...        通过
go test -race ./... 通过
SECKILL_REDIS_IT=1 go test -race ./internal/redisrepo -run TestLuaContracts  通过
```

测试覆盖了并发同键幂等、售罄、活动时间窗、Redis 故障历史回放、Worker 补偿状态、Redis Lua 契约。

## 尚未执行

- `./scripts/setup_db.sh`：会创建数据库并执行 DDL。
- MySQL 集成测试 `SECKILL_IT=1 go test -race ./internal/integration -run TestA -v`。
- 真实压测 `cmd/loadtest`。

按 `AGENTS.md`，执行迁移/写库/压测前需要你确认。若允许，我下一步执行本地建库、A1～A5 集成测试和压测，并修复暴露的问题。

---

## User

根据定义好的边界实现单元测试覆盖和业务流程测试（含异常边界）

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

已完成单元测试与业务流程测试，并使用独立的 `seckill_test` 数据库跑通 MySQL/Redis 集成测试。默认运行库 `seckill` 未被修改。

## 新增测试

- `internal/httpapi/api_test.go`
  - `202` 受理、默认 quantity、缺失用户、非法 payload、限流 `Retry-After`
  - 幂等拒绝回放、查询 `202/409`、订单权限、分页参数
  - 模拟支付、管理员鉴权、`readyz` 503、panic 兜底 500
- `internal/service/service_test.go` / `business_test.go`
  - 50 并发同键只扣一次库存
  - 全局限流不进入 Lua、用户限流只作用于新意图
  - 售罄、队列满、库存未就绪、Redis/MySQL 故障
  - `PROCESSING` / `REJECTED` / `IDEMPOTENCY_CONFLICT` / `RESULT_UNKNOWN`
  - 历史结果优先于 Redis 故障
  - 支付错误映射、活动创建警告、Redis 库存视图、取消回补失败路径
- `internal/worker/worker_test.go`
  - 成功落库回填并 ACK
  - `DONE` 只 ACK、不重复补偿
  - `PENDING` 补偿并标记 DONE
  - 补偿 `MISS`、DB 错误、未知终态保持 pending
- `internal/redisrepo/gateway_test.go`
  - 预扣、重复、冲突、售罄、队列上限不扣库存
  - 旧预约 `MarkSuccess` 不覆盖赢家
  - 补偿/回补标记幂等、无 TTL / 24h TTL、库存键缺失返回 `MISS`
  - 令牌桶限流
- `internal/integration/acceptance_test.go`
  - A1 1000 并发抢 100 库存不超卖
  - A2 同键只建一单、重复重投不新增
  - A2b 首次超限拒绝、首次等于上限成功、后续超限拒绝
  - A3 取消回补一次、Redis 标记回补一次
  - A4 Worker 重启后 `XAUTOCLAIM` 接管，重投不重复建单
  - A5 支付幂等、金额不符、取消后支付拒绝
  - A6 拒绝预约补偿一次，DONE 后不重复补偿
  - A7 服务端到端：下单 → Worker 落库 → 查询 → 支付 → 幂等回放
  - A8 Redis 多放行时 MySQL 条件更新作为最终防线

## 修复的问题

- `mysqlrepo` 未注册 MySQL driver，集成测试启动失败。
- MySQL 会话时区与 `expire_at` UTC 不一致，导致订单被提前判定过期；连接 DSN 自动补充 UTC 会话时区。
- 库存/限购拒绝分支未回填 `ReservationOutcome`，Worker 无法识别补偿。
- 首次购买限购改为“先建 `qty=0` 行，再统一条件更新”，符合最新 SPEC。
- 修正数据库测试中限购用例的预期数量。

## 验证结果

```text
gofmt -l .                        通过
go vet ./...                      通过
go test -race ./...               通过
SECKILL_IT=1 ... go test -race ./internal/integration -v
  8 组集成测试全部通过
SECKILL_REDIS_IT=1 ... go test -race ./internal/redisrepo
  Lua 契约测试通过
```

统计覆盖（`-coverpkg=./internal/...`）：

```text
internal statements coverage: 66.4%
```

未覆盖部分主要是 `cmd/api`、`cmd/loadtest`、`config.Load` 的启动/环境读取逻辑。压测脚本尚未正式执行，1000 RPS / 200 单每秒仍仅为目标值。

---

