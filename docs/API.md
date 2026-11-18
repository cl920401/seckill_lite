# HTTP API 设计

状态：MVP 已实现，详细行为以实现为准；集成测试已使用独立 seckill_test 库验证
事实来源：`docs/SPEC.md`。本文档是接口细节展开；与 SPEC 冲突时先修 SPEC，再同步本文档。

---

## 1. 通用约定

- Base path：`/api/v1`。
- 请求/响应：`application/json; charset=utf-8`；JSON 字段使用 camelCase。
- 时间：请求/响应使用 RFC3339（毫秒）；数据库存 UTC。
- 用户身份：`X-User-Id`，正整数；MVP 信任网关，生产需替换为真实鉴权。
- 管理员身份：`X-Admin-Token`，值来自环境变量，仅用于演示/内网。
- 请求追踪：请求可带 `X-Request-Id`；服务未收到时生成。响应回显。
- 请求体上限：64 KiB。
- 业务错误统一：

```json
{
  "code": "SOLD_OUT",
  "message": "insufficient stock",
  "requestId": "req_01..."
}
```

业务分支不得返回 500；未预期 panic 由中间件兜底为 500 并告警。

### 1.1 `requestId`

建议确定性生成，保证同一用户同一幂等键的重试返回相同值：

```text
requestId = "req_" + lower(hex(sha256(userId + "|" + idempotencyKey)))[0:24]
```

它只用于追踪/日志，不是幂等键；幂等唯一性由 `Idempotency-Key` 与数据库唯一键保证。

### 1.2 `Idempotency-Key`

- 必填，仅对 `POST /api/v1/seckill/orders`。
- 格式：`^[A-Za-z0-9._~-]{8,128}$`，按 `X-User-Id` 隔离。
- 同一用户 + 同一键 + 同一 payload：返回同一历史结果，不重复建单。
- 同一用户 + 同一键 + 不同 payload：`409 IDEMPOTENCY_CONFLICT`。
- 同一键被拒绝（异步或同步）也是稳定终态；用户新的购买意图必须换键。

### 1.3 payload 指纹

请求体仅允许 `itemId`、`quantity`（未知字段拒绝）。指纹：

```text
payloadHash = lower(hex(sha256("v1|" + itemId + "|" + quantity)))
```

对语义相同的 payload 稳定，不受 JSON 空白或字段顺序影响。Redis 与 MySQL 均保存该指纹；比较必须大小写敏感（`ascii_bin`）。

---

## 2. 限流与幂等流程

### 2.1 限流器

| Key | 作用范围 | 初始值（目标/配置，未验证） |
|---|---|---|
| `sk:rl:ingress:global` | 所有 `POST /seckill/orders`，包含幂等回放 | 1000 RPS，burst 1000 |
| `sk:rl:query:global` | 查询接口（按幂等键查询、订单查询/列表） | 2000 RPS，burst 2000 |
| `sk:rl:user:{userId}` | 仅新购买意图，不含幂等回放 | 5 RPS，burst 10 |

阈值来自配置，需用压测校准；当前只是目标/初始配置，不声明已验证。

### 2.2 下单顺序

```text
1. 校验身份、Idempotency-Key、请求体
2. 全局限流 sk:rl:ingress:global
   - 拒绝 -> 429 RATE_LIMITED，不查库、不扣库存
3. 幂等查询（DB 是最终状态）
   a. Redis sk:req 命中 -> 校验 payload 指纹，并查 MySQL 订单/终态确认当前状态
   b. Redis miss/不可用 -> 回源 MySQL orders + request_results
   c. 命中 DB 终态/订单 -> 返回 200
   d. 无 DB 记录且 Redis PROCESSING -> 202
   e. payload 冲突 -> 409 IDEMPOTENCY_CONFLICT
   f. DB 查询失败 -> 503，不进入预扣
4. 确认是“新购买意图”后，执行每用户新单限流 sk:rl:user:{userId}
   - 拒绝 -> 429 RATE_LIMITED
5. 活动/商品/依赖/库存键检查
6. Redis Lua 原子预扣 + XADD
7. 202 Accepted
```

关键语义：

1. **全局限流先于幂等查询**：重复请求也可能被 429。这是基础保护，避免重试打穿数据库。
2. **幂等回放不消耗每用户新单限额**，但仍消耗全局 ingress 限额，且仍会产生一次受保护的历史结果查询（Redis/DB）。
3. **未被限流的同键并发请求不得重复建单**：Redis Lua 只放行一次预扣；Worker 用 `request_results` / `orders` 唯一键兜底。
4. **50 个同键请求同时到达不保证全部 202/200**：部分可能 429；被放行的请求返回一致结果；稍后用同一键重试必须查到同一终态。
5. 幂等回放是只读路径，不写订单、不扣库存。

### 2.3 依赖不可用时的限流

- Redis 不可用：新订单必须 `503 SERVICE_UNAVAILABLE`，不能跳过限流进入 Lua。
- 但历史结果优先：可在**有界的本地并发/超时保护**下回源 MySQL，只读查询已有 `orders` / `request_results`；查到就返回，查不到则 503。
- 建议本地保护参数：`HISTORY_LOOKUP_CONCURRENCY=32`、单次查询超时 200ms；它不是分布式限流，只防止 Redis 故障期间无界打库。
- MySQL 不可用：`503`；不得绕过 Redis 写库。
- `sk:rl:query:global` 不可用时，查询接口可降级到本地并发保护；不得无限查库。

---

## 3. 接口清单

| 方法 | 路径 | 身份 | 说明 |
|---|---|---|---|
| POST | `/api/v1/admin/activities` | Admin | 创建活动与商品库存 |
| GET | `/api/v1/admin/activities/{id}/stock` | Admin | 查询库存（DB 为准，可选 Redis 视图） |
| POST | `/api/v1/seckill/orders` | User | 抢购；需要 `Idempotency-Key` |
| GET | `/api/v1/seckill/requests/{idempotencyKey}` | User | 按幂等键查询受理结果 |
| GET | `/api/v1/orders/{orderNo}` | User | 订单详情 |
| GET | `/api/v1/orders` | User | 当前用户订单列表 |
| POST | `/api/v1/orders/{orderNo}/pay/mock` | User | 模拟支付 |
| GET | `/healthz` | 无 | 进程存活 |
| GET | `/readyz` | 无 | 依赖、Stream group、Worker 就绪 |

---

## 4. 接口契约

### 4.1 创建活动

```http
POST /api/v1/admin/activities
X-Admin-Token: <demo-token>
Content-Type: application/json

{
  "name": "双11秒杀",
  "startAt": "2026-11-18T20:00:00+08:00",
  "endAt": "2026-11-18T22:00:00+08:00",
  "items": [
    {
      "skuCode": "SKU-001",
      "name": "示例商品",
      "priceCents": 9900,
      "totalStock": 100,
      "perUserLimit": 1
    }
  ]
}
```

校验：

- `name` 1～128 字符；`startAt < endAt`；
- 至少一个 item；同一活动 `skuCode` 唯一；
- `priceCents >= 0`；`totalStock > 0`；`1 <= perUserLimit <= totalStock`。

成功：

```http
201 Created

{
  "activityId": 1,
  "items": [
    {
      "itemId": 10,
      "skuCode": "SKU-001",
      "availableStock": 100,
      "redisReady": true
    }
  ],
  "warnings": []
}
```

部分失败：

- MySQL 事务已提交、Redis 库存键写入失败时，仍返回 `201`，对应 item `redisReady=false`，并返回 warning；不回滚活动、不猜测库存。
- 在该商品库存键存在前，下单返回 `503 NOT_READY`。

### 4.2 查询库存

```http
GET /api/v1/admin/activities/1/stock?includeRedis=true
X-Admin-Token: <demo-token>
```

```json
{
  "activityId": 1,
  "items": [
    {
      "itemId": 10,
      "totalStock": 100,
      "availableStock": 80,
      "version": 20,
      "redis": {
        "ready": true,
        "stock": 80
      }
    }
  ]
}
```

- `availableStock` 永远来自 MySQL，是事实来源。
- `redis` 只有显式 `includeRedis=true` 才返回；用于演示/排查，不可作为最终结果。

### 4.3 抢购下单

```http
POST /api/v1/seckill/orders
X-User-Id: 10001
Idempotency-Key: 3f8a9c2e-...
Content-Type: application/json

{"itemId": 42, "quantity": 1}
```

`quantity` 可省略，默认 1；必须为正整数。

首次受理：

```http
202 Accepted
Retry-After: 1

{
  "requestId": "req_...",
  "idempotencyKey": "3f8a9c2e-...",
  "status": "PROCESSING",
  "pollUrl": "/api/v1/seckill/requests/3f8a9c2e-...",
  "retryAfterMs": 500
}
```

幂等命中成功：

```http
200 OK

{
  "requestId": "req_...",
  "idempotencyKey": "3f8a9c2e-...",
  "status": "SUCCEEDED",
  "orderNo": "SK202611180001",
  "orderStatus": "PENDING_PAYMENT",
  "itemId": 42,
  "quantity": 1,
  "expireAt": "2026-11-18T22:30:00+08:00"
}
```

幂等命中异步拒绝：

```http
200 OK

{"status":"REJECTED","reason":"LIMIT_EXCEEDED"}
```

同步预检拒绝（未受理，不产生 Stream 消息）：`409` 业务错误。异步拒绝只在受理后出现，重试时以 `200 REJECTED` 返回稳定终态。

### 4.4 按幂等键查询

```http
GET /api/v1/seckill/requests/3f8a9c2e-...
X-User-Id: 10001
```

| 场景 | HTTP | 响应 |
|---|---|---|
| 已受理、Worker 未处理 | 202 | `status: PROCESSING` + `retryAfterMs` |
| 已落库 | 200 | `status: SUCCEEDED` + 订单摘要 |
| 持久 REJECTED | 200 | `status: REJECTED` + `reason` |
| DB 无记录、Redis 无记录 | 409 | `RESULT_UNKNOWN` |
| Redis 不可用且 DB 无记录 | 503 | 无法确认 |
| 请求过频 | 429 | `RATE_LIMITED` + `Retry-After` |

`RESULT_UNKNOWN` 不是“失败”，不得据此回补库存；按 SPEC 7.3 人工核查。

### 4.5 订单查询

```http
GET /api/v1/orders/SK202611180001
X-User-Id: 10001
```

```json
{
  "orderNo": "SK202611180001",
  "userId": 10001,
  "activityId": 1,
  "itemId": 42,
  "quantity": 1,
  "amountCents": 9900,
  "status": "PENDING_PAYMENT",
  "expireAt": "2026-11-18T22:30:00+08:00",
  "paidAt": null,
  "createdAt": "2026-11-18T22:15:00+08:00"
}
```

订单不存在或不属于当前用户：统一 `404 ORDER_NOT_FOUND`。

### 4.6 订单列表

```http
GET /api/v1/orders?limit=20&beforeId=12345
X-User-Id: 10001
```

```json
{
  "items": [
    {
      "orderNo": "SK202611180001",
      "itemId": 42,
      "quantity": 1,
      "amountCents": 9900,
      "status": "PENDING_PAYMENT",
      "createdAt": "2026-11-18T22:15:00+08:00"
    }
  ],
  "nextBeforeId": 12340
}
```

- 按订单 ID 倒序，`beforeId` 为上一页最后一条的 ID。
- `limit` 默认 20，最大 100。
- 不以任意 `userId` 查询；如需管理员查询，另做受保护运维接口。若收到 `userId` 参数且不等于 `X-User-Id`，返回 `400 INVALID_ARGUMENT`。

### 4.7 模拟支付

```http
POST /api/v1/orders/SK202611180001/pay/mock
X-User-Id: 10001
Content-Type: application/json

{"providerTxnId":"mock-txn-1","amountCents":9900}
```

```json
{
  "orderNo": "SK202611180001",
  "status": "PAID",
  "paidAt": "2026-11-18T22:16:00+08:00",
  "providerTxnId": "mock-txn-1"
}
```

- 校验订单归属与金额；金额不一致返回 `400 INVALID_ARGUMENT`。
- 重复同一 `providerTxnId` 返回幂等成功。
- 已取消订单返回 `409 PAY_AFTER_CANCEL`，不退款。
- 丢失回调不自动对账；订单可超时取消，或由用户重试同一模拟支付。

### 4.8 健康检查

- `GET /healthz`：进程存活，返回 200。
- `GET /readyz`：检查 MySQL ping、Redis ping、Stream 消费组、Worker 状态；任一不满足返回 503。
- `/readyz` 不扫描全部库存键；库存键缺失是单品 `503 NOT_READY`，不是全服务未就绪。

---

## 5. 错误码

| HTTP | code | 语义 |
|---|---|---|
| 400 | `INVALID_ARGUMENT` | 参数/幂等键/金额/分页参数非法 |
| 401 | `UNAUTHENTICATED` | 缺少用户或管理员凭证 |
| 404 | `ITEM_NOT_FOUND` | 商品不存在 |
| 404 | `ORDER_NOT_FOUND` | 订单不存在或不属于当前用户 |
| 409 | `ACTIVITY_NOT_STARTED` | 活动未开始 |
| 409 | `ACTIVITY_ENDED` | 活动已结束/禁用 |
| 409 | `SOLD_OUT` | 库存不足（Redis 或 DB 判定） |
| 409 | `LIMIT_EXCEEDED` | 异步落库发现超过限购 |
| 409 | `IDEMPOTENCY_CONFLICT` | 同键不同 payload |
| 409 | `RESULT_UNKNOWN` | 受理结果无法确认；禁止回补 |
| 409 | `PAY_AFTER_CANCEL` | 已取消订单收到支付 |
| 429 | `RATE_LIMITED` | 触发用户/全局/查询限流 |
| 429 | `QUEUE_FULL` | Stream 条目达到上限，未受理 |
| 503 | `SERVICE_UNAVAILABLE` | Redis/MySQL 不可用、熔断 |
| 503 | `NOT_READY` | 库存键未初始化/未就绪 |

429/503 建议带 `Retry-After` 秒数。业务错误不得伪装成 500。

---

## 6. 状态与响应一致性

请求级：

```text
PROCESSING -> SUCCEEDED
           -> REJECTED
           -> UNKNOWN（查询时用 409 表达）
```

订单级：

```text
PENDING_PAYMENT -> PAID
                -> CANCELLED
```

- 支付与取消通过条件更新竞争，只有一个成功。
- 同一幂等键在活动结束、售罄、Redis 幂等键被删除后再次请求，仍以 MySQL 历史结果为准；不得改判失败。
- 同一键被拒绝后，新的购买意图必须使用新键。

---

## 7. 验收口径调整（A2）

- 50 个同键同 payload 并发请求：允许部分返回 `429 RATE_LIMITED`；**未被限流的请求不得创建重复订单**。
- 所有被放行的请求返回 `202` 或同一终态；不允许 500、不允许两个 `orderNo`。
- 等待限流窗口后，用同一键顺序重试：必须返回同一 `orderNo` 与状态；改 payload 返回 `409 IDEMPOTENCY_CONFLICT`。
- 数据库断言：`UNIQUE(user_id, idempotency_key)` 下只有一条订单；`request_results` 只有一行。
- 幂等回放不消耗 `sk:rl:user:{userId}` 新单令牌，但仍消耗全局 ingress；因此不能承诺“无限重试全部成功”。

---

## 8. 已知限制

- 全局限流是基础保护，不是精确容量控制；阈值需实测校准。
- 幂等回放仍可能触发 DB 查询；Redis 故障时仅靠本地并发/超时保护，不能提供分布式级读保护。
- 同键不同 payload 的冲突在 Redis 丢失且 DB 尚无终态的窗口内无法完全提前识别；最终由 `request_results` 锁定的胜者决定，旧预约进入额外预约补偿。
- `sk:restocked` 只有 24 小时标记，不是任意时间的重复回补保证；见 `docs/REDIS_KEYS.md` 和 SPEC 7.3。
- 不实现自动对账、自动库存重建、真实退款。
