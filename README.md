# seckill_lite

Go + MySQL + Redis 轻量级秒杀系统。

## 文档

- 需求、状态机与验收：[docs/SPEC.md](docs/SPEC.md)
- HTTP 接口：[docs/API.md](docs/API.md)
- 表结构与事务协议：[docs/SCHEMA.md](docs/SCHEMA.md) / [migrations/0001_init.sql](migrations/0001_init.sql)
- Redis Key 与 Lua 契约：[docs/REDIS_KEYS.md](docs/REDIS_KEYS.md)
- 测试边界与执行记录：[docs/TEST_PLAN.md](docs/TEST_PLAN.md)
- Prompt/决策记录：[docs/PROMPT_LOG.md](docs/PROMPT_LOG.md)

## 当前状态（2026-11-19 最终验收）

已实现 MVP 代码：

- 活动/商品创建与库存初始化
- Redis Lua 预扣库存 + Redis Stream 异步受理
- Worker 幂等落库（`request_results`、`reservation_results`、条件扣库存）
- 订单查询、按幂等键查询、模拟支付
- 超时取消与 DB/Redis 库存回补
- 基础用户/全局/查询限流
- 单元测试、真实 MySQL/Redis 集成测试、Lua 契约测试、压测/演示脚本

本轮**实际执行并验证**：

- `gofmt -l .`、`go vet ./...`、`go build ./...` 通过
- `go test -race -count=1 ./...` 通过
- Lua 契约（Redis 逻辑库 db14）通过，含补偿部分执行 `BAD_STOCK` 用例
- 集成测试 25/25 通过（MySQL `seckill_test` + Redis db14），覆盖并发不超卖、同键幂等、取消回补一次、重投/补偿窗口、提交结果不明、补偿部分执行、依赖故障/结果不明、Fix-3/Fix-7/Fix-8 回归
- `scripts/demo_final.sh` 最小演示通过：创建活动、并发 200 请求抢 50 库存、查询、支付、幂等冲突、超时取消

**未验证 / 已知限制**：

- 1000 入口 RPS / 200 新订单每秒是目标值，**未实测**，不得写成结论。
- 真实停服/杀进程、HTTP 真栈、多实例并发认领、真实 24h TTL、指标平台未验证。
- Fix-2（Worker 退避/`attempts`/阈值告警）与 Fix-4（Worker 旧快照读）仍待处理。
- 取消后 Redis 回补失败仅告警、不自动重试；无监控平台，仅结构化日志。
- 本轮测试数据仅写入 `seckill_test` 与 Redis db14；未触碰运行库 `seckill_lite`/db0。

## 前置条件

- Go 1.27.4+（以 `go.mod` 的 `go 1.27.4` 为准）
- MySQL 8.0.16+ / 8.4
- Redis 6.2+（使用 Stream、XAUTOCLAIM）
- 本机可直接访问 MySQL 与 Redis；不提供自动安装

## 启动

1. 创建数据库并执行 DDL（需要用户确认后执行）：

```bash
./scripts/setup_db.sh
```

默认使用 `root@127.0.0.1:3306` 与数据库 `seckill`。可通过 `MYSQL_USER`、`MYSQL_HOST`、`MYSQL_PORT`、`MYSQL_DATABASE` 覆盖。密码鉴权需自行调整脚本或使用 `MYSQL_DSN` 启动 API。

2. 启动服务：

```bash
cp .env.example .env
set -a; source .env; set +a
go run ./cmd/api
```

服务默认监听 `:8080`。启动时会确保 Redis Stream 消费组存在；不会覆盖已有库存键。

3. 健康检查：

```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

## 最小演示

推荐使用 `scripts/demo_final.sh`（先启动服务并设置测试库/Redis 环境）：

```bash
BASE_URL=http://127.0.0.1:8080 \
ADMIN_TOKEN=demo-admin-token \
MYSQL_DB=seckill_test \
MYSQL_DSN='root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4' \
REDIS_DB=14 REDIS_PREFIX='sk:demo:final:' \
./scripts/demo_final.sh
```

脚本覆盖：创建活动 → 下单/轮询 → 模拟支付 → 同键不同 payload 返回 `IDEMPOTENCY_CONFLICT` → 200 并发抢 50 库存并校验订单/库存 → 超时取消并校验 DB/Redis 各回补一次。以下为手动 curl 示例。

创建活动（演示令牌来自 `ADMIN_TOKEN`）：

```bash
curl -s -X POST localhost:8080/api/v1/admin/activities \
  -H 'X-Admin-Token: replace-with-demo-token' \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "demo",
    "startAt": "2026-01-01T00:00:00Z",
    "endAt": "2030-01-01T00:00:00Z",
    "items": [{"skuCode":"SKU-1","name":"demo item","priceCents":100,"totalStock":100,"perUserLimit":1}]
  }'
```

抢购（`<itemId>` 替换为响应中的 itemId）：

```bash
curl -i -X POST localhost:8080/api/v1/seckill/orders \
  -H 'X-User-Id: 10001' \
  -H 'Idempotency-Key: demo-key-0001' \
  -H 'Content-Type: application/json' \
  -d '{"itemId": 1, "quantity": 1}'
```

返回 `202` 只表示受理。用同一幂等键查询：

```bash
curl -i localhost:8080/api/v1/seckill/requests/demo-key-0001 \
  -H 'X-User-Id: 10001'
```

重复使用相同用户、相同幂等键和相同 payload 不会重复建单；并发重试可能返回 `429`，稍后重试同一键应得到一致结果。

## 测试与压测

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

默认单元测试不连接外部服务。真实 MySQL/Redis 集成测试需要先建独立测试库，并显式开启：

```bash
mysql -uroot -e "CREATE DATABASE seckill_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"
mysql -uroot seckill_test < migrations/0001_init.sql

SECKILL_IT=1 \
SECKILL_IT_DSN='root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4' \
SECKILL_IT_REDIS='127.0.0.1:6379' \
SECKILL_IT_REDIS_DB=14 \
go test -race -p 1 ./internal/integration -v
```

集成测试会拒绝非 `*_test` 数据库；Redis 必须显式指定独立逻辑库（本轮使用 db14，测试用唯一前缀并在结束时清理）。测试数据只写入测试库与测试 Redis，不连接运行库。

数据库连接会自动补充 UTC 会话时区，保证 `expire_at` 与 `CURRENT_TIMESTAMP` 使用同一时钟。

Redis Lua 契约测试：

```bash
SECKILL_REDIS_IT=1 \
SECKILL_IT_REDIS='127.0.0.1:6379' \
SECKILL_IT_REDIS_DB=14 \
go test -race ./internal/redisrepo -v
```

含补偿部分执行（`BAD_STOCK`）用例，验证库存值异常时不会误设补偿标记。

压测入口（先创建相同 itemId 的活动）：

```bash
go run ./cmd/loadtest -base http://127.0.0.1:8080 -n 1000 -c 200 -item <itemId> -activity <activityId> -mode unique
```

可选只读校验（使用测试库）：

```bash
go run ./cmd/loadtest -base http://127.0.0.1:8080 -n 1000 -c 200 -item <itemId> -mode unique \
  -dsn 'root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4'
```

报告中的数值是目标/实测值，必须注明机器、版本与参数；当前**未进行正式 RPS 压测**，1000 RPS / 200 单每秒不是已验证结论。

## 关键限制

- 单 Redis、单 MySQL；无自动对账、自动库存重建、自动故障转移。
- Redis 数据丢失可能丢失未落库预约、幂等状态、Stream 和补偿标记；需人工核查。
- `sk:restocked` 只有 24h 标记，不是任意时间重复回补的安全保证；标记过期后必须暂停并核对 DB/Redis。
- 幂等回放仍受全局限流，允许 `429`；未被限流的同键请求不得重复建单。
- 不实现真实支付/退款；模拟支付回调丢失不自动对账。
- Stream 达到 10000 条上限后返回 `429 QUEUE_FULL`，需人工确认无 pending 后清理。
- 补偿/回补 Lua 在库存值非整数时返回 `BAD_STOCK`，不设置标记、不 `INCRBY`；消息保留 pending 并告警，需人工核查。
- 取消后 Redis 回补失败只记录告警，不自动重试；MySQL 已回补，Redis 可能暂时少卖。
- 无监控平台，仅结构化日志；Worker 退避/`attempts` 与旧快照读问题（Fix-2/Fix-4）仍待处理。
- 本轮验收未执行：真实停服/`SIGKILL`、HTTP 真栈、多实例并发认领、真实 24h TTL、正式 RPS 压测。
- 演示数据写入 `seckill_test` 与 Redis db14；如需清理由维护者确认后执行。
- 1000 入口 RPS / 200 新订单每秒是目标值，不是已验证结论。
