# seckill_lite

Go + MySQL + Redis 轻量级秒杀系统。

## 文档

- 需求、状态机与验收：[docs/SPEC.md](docs/SPEC.md)
- HTTP 接口：[docs/API.md](docs/API.md)
- 表结构与事务协议：[docs/SCHEMA.md](docs/SCHEMA.md) / [migrations/0001_init.sql](migrations/0001_init.sql)
- Redis Key 与 Lua 契约：[docs/REDIS_KEYS.md](docs/REDIS_KEYS.md)

## 当前状态

已实现 MVP 代码：

- 活动/商品创建与库存初始化
- Redis Lua 预扣库存 + Redis Stream 异步受理
- Worker 幂等落库（`request_results`、`reservation_results`、条件扣库存）
- 订单查询、按幂等键查询、模拟支付
- 超时取消与 DB/Redis 库存回补
- 基础用户/全局/查询限流
- 单元测试、可选的真实 MySQL/Redis 集成测试、压测脚本

测试库 `seckill_test` 已执行 DDL 并通过集成测试；默认运行库迁移、正式压测、长期运行验证尚未执行。不要将目标值 1000 RPS / 200 单每秒视为已验证结果。

## 前置条件

- Go 1.22+（本仓库在 Go 1.27 环境开发）
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
go test -race ./internal/integration -run TestA -v
```

数据库连接会自动补充 UTC 会话时区，保证 `expire_at` 与 `CURRENT_TIMESTAMP` 使用同一时钟。

Redis Lua 契约测试：

```bash
SECKILL_REDIS_IT=1 go test ./internal/redisrepo -run TestLuaContracts -v
```

压测入口（先创建相同 itemId 的活动）：

```bash
go run ./cmd/loadtest -n 1000 -c 200 -item 1 -activity 1 -mode unique
```

可选只读校验：

```bash
go run ./cmd/loadtest -n 1000 -c 200 -item 1 -mode unique \
  -dsn 'root@tcp(127.0.0.1:3306)/seckill?parseTime=true&loc=UTC&charset=utf8mb4'
```

报告中的数值是目标/实测值，必须注明机器、版本与参数；当前未进行正式压测。

## 关键限制

- 单 Redis、单 MySQL；无自动对账、自动库存重建、自动故障转移。
- Redis 数据丢失可能丢失未落库预约、幂等状态、Stream 和补偿标记；需人工核查。
- `sk:restocked` 只有 24h 标记，不是任意时间重复回补的安全保证；标记过期后必须暂停并核对 DB/Redis。
- 幂等回放仍受全局限流，允许 `429`；未被限流的同键请求不得重复建单。
- 不实现真实支付/退款；模拟支付回调丢失不自动对账。
- Stream 达到 10000 条上限后返回 `429 QUEUE_FULL`，需人工确认无 pending 后清理。
- 1000 入口 RPS / 200 新订单每秒是目标值，不是已验证结论。
