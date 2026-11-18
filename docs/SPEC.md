# 秒杀系统 MVP 规格（SPEC）

状态：已收敛，待确认后实现  
目标周期：4～6 小时可运行、可测试、可演示的 MVP  
技术栈：Go + MySQL + Redis（单实例）  
本文档是本轮范围的唯一事实来源；如与 README、API、DDL 冲突，以本文档为准并只做必要同步。

---

## 1. 文档同步说明

- 仓库当前只有 `go.mod` 与仅含标题的 `README.md`，没有已存在的 API 文档或 DDL，因此不存在需要迁移或改写的旧接口/表结构。
- 此前对话中的“同步下单”方案被本 SPEC 取代：本 MVP 使用 **Redis Stream 异步受理**，入口返回 `202`，订单由 Worker 落库。
- README 只补充项目说明和本文档链接，不删除原有内容。
- 原题完整保留在附录 A，未删除项目文件或已有数据。
- 本轮只写文档，不写业务代码，不执行任何数据库操作；DDL 在实现阶段新增迁移文件。

---

## 2. 目标与非目标

### 2.1 MVP 目标（必须实现并验证）

1. **活动与库存初始化**：创建活动、活动时间窗、商品/SKU、总库存、每人限购数；初始化 MySQL 库存与 Redis 库存键。
2. **异步秒杀下单**：
   - 入口完成参数、活动状态、限流、幂等预检；
   - Redis Lua 原子完成“幂等检查 + 扣减 Redis 库存 + 写入 Stream”；
   - 入口返回 `202 Accepted`，表示“已受理”，不表示下单成功；
   - Worker 消费 Stream，在 MySQL 事务中使用条件更新防超卖并创建订单。
3. **订单查询**：按订单号查询、按用户查询订单列表；按幂等键查询受理结果（支持 `202` 轮询）。
4. **模拟支付**：待支付订单可被模拟支付回调置为已支付；支付回调幂等；不做真实退款。
5. **超时取消与库存回补**：待支付订单超时后取消，MySQL 与 Redis 库存各回补一次；取消逻辑可重复执行而不重复回补。
6. **最小故障语义**：
   - Redis 或 MySQL 不可用时，新订单快速失败，绝不绕过 Redis 直接写库下单；
   - 已受理但结果不明的请求保留消息，可用同一幂等键查询/重试；
   - 任何情况下都不因“超时”或“查不到订单”自动回补库存。
7. **可测试**：并发不超卖、幂等、超时取消回补一次、Worker 重启消息重试、依赖不可用行为均有自动化或可重复脚本验证。

### 2.2 非目标（后续方案，不在 MVP 内实现，也不声称已具备）

- 自动对账、自动库存修复、自动 Redis 重建、自动故障转移/高可用。
- 真实支付网关、真实退款、支付对账文件。
- 库存分片、热点商品多行库存、Kafka、跨机房、多活。
- 自动 DLQ 修复、复杂补偿状态机、人工工单系统。
- 已验证的 1000 入口 RPS / 200 订单每秒；这是目标值，不是实测结论。
- 多租户、完整鉴权、风控、防刷设备指纹。
- Redis Cluster / MySQL 主从 / 容器编排。

### 2.3 已知约束

- 单 Redis、单 MySQL、单应用实例（Worker 与 API 可在同一进程）。
- Redis 使用 AOF `everysec` 或等效持久化只能降低丢失窗口，**不能保证不丢数据**。
- Redis 数据丢失可能造成“已预扣但未落库”的预约丢失；MySQL 中已存在的订单不受影响。
- 入口的幂等状态、Redis 库存、Stream 消息均可能因 Redis 故障或数据丢失而消失；系统不自动推断并回补。

---

## 3. 架构与边界

```text
Client
  |
  v
HTTP API
  |-- 参数/身份/幂等键校验
  |-- 限流（每用户/全局）
  |-- 活动/商品快照校验
  |-- 依赖健康检查（Redis + MySQL）
  |
  v
Redis Lua（单脚本原子执行）
  |-- 幂等键存在？ -> 返回已有状态
  |-- 库存不足？   -> 返回 SOLD_OUT
  |-- DECRBY 库存
  |-- XADD 预约消息到 Stream
  |-- SET 幂等状态 = PROCESSING（带 payload 指纹）
  |
  v
202 Accepted
  |
  v
Redis Stream（consumer group）
  |
  v
Order Worker
  |-- XREADGROUP 拉取；不立即 XACK
  |-- MySQL 事务：幂等检查 + 条件扣库存 + 限购 + 建订单 + 流水
  |-- 成功后 XACK；失败保留 pending 重试/告警
  |-- 尽力更新 Redis 幂等状态为 SUCCEEDED/REJECTED
  |
  v
MySQL（唯一事实来源）
  |
  +--> 订单查询 / 模拟支付 / 超时取消
```

### 3.1 各层职责

- **HTTP 层**：不做库存写操作，不直接写订单；只做校验、限流、调用服务。
- **Redis 层**：高并发闸门、异步削峰、幂等快速返回。不是最终事实来源。
- **MySQL 层**：订单、库存、限购、支付、取消的最终事实来源；条件更新承担最终防超卖。
- **Worker**：唯一的订单落库入口；负责 Stream 消费、重试、幂等兜底。

### 3.2 为什么用异步

入口只做 O(1) 的 Redis Lua 操作并投递消息，避免把入口并发直接放大到 MySQL；Worker 按可控并发消费。代价是接口从“同步成功”变为“202 受理 + 轮询结果”，客户端必须理解该语义。

---

## 4. 业务流程

### 4.1 活动与库存初始化

1. 运营接口创建活动：名称、`start_at`、`end_at`、状态。
2. 同一请求创建商品项：SKU 标识、价格（分）、`total_stock`、`available_stock = total_stock`、`per_user_limit`。
3. MySQL 事务提交后，写入 Redis：
   - `sk:stock:{itemID}` = `available_stock`
   - 活动/商品快照缓存（TTL 短，例如 5～10 秒）
4. 若 Redis 写入失败：接口返回“活动已创建但未就绪”的业务状态；在库存键存在前，该商品的下单请求一律 `503 SERVICE_UNAVAILABLE`，不自动猜测库存。
5. 服务启动时仅对活跃活动做一次“按 MySQL 加载库存键”的预加载；这是启动预热，不是运行期自动对账或自动重建。库存键缺失时下单快速失败并告警。

### 4.2 抢购下单（入口）

前置条件：请求必须带用户标识与幂等键。

1. 校验 `Idempotency-Key`：必填、长度 8～128、字符集受限、按用户隔离。
2. 限流检查：每用户 + 全局；超限返回 `429 RATE_LIMITED`，不扣 Redis 库存。
3. 校验活动/商品：存在、活动已开始、未结束、数量合法。
4. 依赖检查：Redis 与 MySQL 均不可用或熔断时返回 `503 SERVICE_UNAVAILABLE`，不进入 Lua、不写库。
5. 执行一个 Redis Lua 脚本，原子完成：
   - 读取 `sk:req:{userID}:{idempotencyKey}`：
     - 已存在且 payload 指纹一致 → 返回已有状态（`PROCESSING` / `SUCCEEDED` / `REJECTED`）；
     - 已存在但 payload 不同 → 返回 `IDEMPOTENCY_CONFLICT`；
   - 读取库存键：
     - 库存键不存在 → 返回“未就绪”，接口 `503`，不初始化、不扣减；
     - 库存不足 → 返回 `SOLD_OUT`；
   - `DECRBY` 库存；
   - `XADD sk:stream:orders` 写入预约消息；
   - `SET sk:req:{...}` = `PROCESSING`，保存 payload 指纹、时间，TTL 覆盖最大处理与查询窗口（例如 24 小时）；
   - 返回剩余库存与受理状态。
6. 返回：
   - 首次受理：`202` + `requestId`、`idempotencyKey`、`status: PROCESSING`、轮询地址；
   - 幂等命中处理中：`202`，与首次语义一致；
   - 幂等命中已完成：`200` + 订单结果；
   - 同步预检失败（活动未开始/已结束/售罄等）：明确业务错误码，不创建 Stream 消息。

**重要语义**：`202` 只代表“已受理并进入队列”，不代表扣库存成功或订单已创建。客户端必须轮询或重试查询。

### 4.3 Worker 落库

每条 Stream 消息字段至少包含：

```text
idempotencyKey, userId, itemId, quantity, payloadHash, acceptedAt
```

处理步骤：

1. `XREADGROUP GROUP order-workers CONSUMER worker-{instance} COUNT n BLOCK ms STREAMS sk:stream:orders >`
2. 开启 MySQL 事务：
   1. 按 `(user_id, idempotency_key)` 查订单：
      - 已存在且 payload 一致 → 视为重复消息；补偿本条消息对应的 Redis 预扣（按 Stream message ID 记一次性补偿标记），`XACK`，不重复扣 DB 库存；
      - 已存在但 payload 冲突 → 不写库、不覆盖；标记 `IDEMPOTENCY_CONFLICT` 并告警，`XACK` 该消息，查询该幂等键返回冲突语义。
   2. 条件扣库存：
      ```sql
      UPDATE seckill_items
         SET available_stock = available_stock - ?, version = version + 1
       WHERE id = ? AND available_stock >= ?
      ```
      影响行数为 0 → 视为售罄，回滚事务。
   3. 限购：在事务内以行锁/条件更新保证 `user_purchases.qty + quantity <= per_user_limit`；不满足则回滚。
   4. 插入订单：`status = PENDING_PAYMENT`、`expire_at = now + TTL`；依赖 `UNIQUE(user_id, idempotency_key)` 兜底幂等。
   5. 写库存流水（`DEDUCT`）与必要的审计字段。
   6. 提交事务。
3. 提交成功后：
   - 更新 Redis 幂等状态为 `SUCCEEDED`（尽力而为；失败不影响订单查询，因为 DB 可按幂等键回查）；
   - `XACK` 消息。
4. 提交失败：
   - 不 `XACK`，消息留在 pending，按退避重试；
   - 瞬时错误（连接、死锁、超时）持续重试；
   - 达到告警阈值仍无法确认时，暂停该消息处理并告警，等待人工核查；不能把“未确认”当成失败回补库存。
5. 售罄/限购拒绝：
   - 回滚事务（DB 库存不变）；
   - 尽力将 Redis 幂等状态置为 `REJECTED` 并记录原因；
   - 对入口已预扣的 Redis 库存做“带标记的一次性回补”（见 6.4）；
   - `XACK` 消息。
6. Worker 启动或周期性执行 `XAUTOCLAIM`，接管空闲超过阈值（例如 60 秒）的 pending 消息；重复消息由 DB 唯一键与订单回查保证幂等。

### 4.4 查询

- 按订单号：只读 MySQL；订单不存在返回 `404 ORDER_NOT_FOUND`。
- 按用户：只读 MySQL，分页。
- 按幂等键（受理结果查询）：
  1. 先查 MySQL 订单 `(user_id, idempotency_key)`；
  2. 若存在 → `200` + 订单状态；
  3. 不存在 → 查 Redis `sk:req:{...}`：
     - `PROCESSING` → `202` + 建议轮询间隔；
     - `REJECTED` → `200` + `status: REJECTED` + 原因；
     - 不存在 → `409 RESULT_UNKNOWN`：可能是从未受理，也可能是 Redis 数据丢失。**不得据此判定失败或回补库存**。
  4. Redis 不可用且 DB 无订单 → `503`，不返回 `404`。

### 4.5 模拟支付

- `POST /api/v1/orders/{orderNo}/pay/mock`，请求携带 `providerTxnId` 与金额。
- 事务内条件更新：`status = PAID WHERE order_no = ? AND status = PENDING_PAYMENT`。
- 同一 `providerTxnId`/同一订单重复回调：幂等成功，返回已支付结果。
- 订单已取消：返回 `409 PAY_AFTER_CANCEL`，仅记录，不做真实退款。
- 支付记录使用 `UNIQUE(order_no, provider_txn_id)` 防重。
- 丢失回调：订单保持待支付，最终可能超时取消；用户可重新触发同一笔模拟支付。MVP **不实现自动支付对账**。

### 4.6 超时取消与回补

独立调度循环按索引扫描 `status = PENDING_PAYMENT AND expire_at <= now`：

1. 取一批订单（例如 100 条），使用 `SELECT ... FOR UPDATE SKIP LOCKED` 或等价的并发保护。
2. 单订单事务：
   - `UPDATE orders SET status = CANCELLED WHERE id = ? AND status = PENDING_PAYMENT`；影响 0 行说明已被支付/取消，直接跳过；
   - 回补 MySQL `available_stock`；
   - 回补 `user_purchases.qty`；
   - 写 `stock_logs(order_no, 'CANCEL', +qty)`，依赖 `UNIQUE(order_no, type)` 防重复记账；
   - 提交。
3. 提交后执行 Redis 一次性回补 Lua：
   - `SET sk:restocked:{orderNo} 1 NX EX ...` 成功才 `INCRBY sk:stock:{itemID} qty`；
   - 若 Redis 不可用：不做自动恢复，记录失败、指标与告警；MySQL 库存已正确回补，Redis 可能暂时少卖，需人工处理。
4. 对于“Redis 有 PROCESSING 状态、但 DB 中没有订单且超过处理宽限期”的请求：
   - **不自动取消、不回补**；
   - 标记疑似异常，告警并暂停该商品的自动处理；
   - 按第 7.3 节人工核查。

**取消只针对已落库的过期订单**，不针对“查不到订单”的请求。

---

## 5. 接口与状态

### 5.1 接口清单

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/admin/activities` | 创建活动与商品库存（运营） |
| GET | `/api/v1/admin/activities/{id}/stock` | 查 MySQL 剩余库存（运营） |
| POST | `/api/v1/seckill/orders` | 抢购；需要 `X-User-Id` 与 `Idempotency-Key` |
| GET | `/api/v1/seckill/requests/{idempotencyKey}` | 按幂等键查询受理结果；需要 `X-User-Id` |
| GET | `/api/v1/orders/{orderNo}` | 订单详情 |
| GET | `/api/v1/orders?userId=...` | 用户订单列表 |
| POST | `/api/v1/orders/{orderNo}/pay/mock` | 模拟支付 |
| GET | `/healthz` | 进程存活 |
| GET | `/readyz` | 依赖/库存预热就绪 |

身份：MVP 用 `X-User-Id` 模拟网关身份，生产需替换为真实鉴权。

### 5.2 下单请求与响应

```http
POST /api/v1/seckill/orders
X-User-Id: 10001
Idempotency-Key: 3f8a9c2e-...
Content-Type: application/json

{"itemId": 42, "quantity": 1}
```

首次受理：

```http
202 Accepted

{
  "requestId": "req_...",
  "idempotencyKey": "3f8a9c2e-...",
  "status": "PROCESSING",
  "poll": "/api/v1/seckill/requests/3f8a9c2e-..."
}
```

幂等命中已完成：

```http
200 OK

{
  "orderNo": "SK202611180001",
  "status": "PENDING_PAYMENT",
  "expireAt": "2026-11-18T22:30:00+08:00"
}
```

幂等命中被拒绝（异步阶段）：

```http
200 OK

{"status": "REJECTED", "reason": "LIMIT_EXCEEDED"}
```

同一幂等键、不同请求体：

```http
409 Conflict

{"code": "IDEMPOTENCY_CONFLICT", "message": "idempotency key reused with different payload"}
```

### 5.3 业务错误码

| HTTP | code | 语义 |
|---|---|---|
| 400 | `INVALID_ARGUMENT` | 参数/幂等键非法 |
| 401 | `UNAUTHENTICATED` | 缺少用户标识 |
| 404 | `ITEM_NOT_FOUND` | 商品不存在 |
| 404 | `ORDER_NOT_FOUND` | 订单号不存在（仅订单号查询） |
| 409 | `ACTIVITY_NOT_STARTED` | 活动未开始 |
| 409 | `ACTIVITY_ENDED` | 活动已结束 |
| 409 | `SOLD_OUT` | 库存不足（Redis 或 DB 判定） |
| 409 | `LIMIT_EXCEEDED` | 异步落库时发现超过限购 |
| 409 | `IDEMPOTENCY_CONFLICT` | 同一幂等键不同 payload |
| 409 | `RESULT_UNKNOWN` | 受理结果无法确认；禁止据此回补 |
| 409 | `PAY_AFTER_CANCEL` | 已取消订单收到支付 |
| 429 | `RATE_LIMITED` | 触发限流 |
| 503 | `SERVICE_UNAVAILABLE` | Redis/MySQL 不可用、库存未就绪、熔断 |
| 503 | `NOT_READY` | 库存键未预热完成 |

业务错误统一返回 `{code, message, requestId}`；**业务分支不得返回 500**。未预期 panic 由中间件兜底为 500 并告警，但验收用例必须覆盖上述业务错误。

### 5.4 状态机

请求级（Redis 幂等记录）：

```text
PROCESSING -> SUCCEEDED
           -> REJECTED
           -> (记录丢失/无法确认) -> UNKNOWN（查询时由 409 表达）
```

订单级（MySQL）：

```text
PENDING_PAYMENT -> PAID
                -> CANCELLED
PAID / CANCELLED 为终态；支付与取消通过条件更新竞争，只有一个成功。
```

### 5.5 查询状态语义

| 场景 | HTTP | 结果 |
|---|---|---|
| 已受理、Worker 未处理 | 202 | `PROCESSING` |
| 已落库 | 200 | `PENDING_PAYMENT` / `PAID` / `CANCELLED` |
| 异步拒绝 | 200 | `REJECTED` + 原因 |
| Redis 无记录、DB 无订单 | 409 | `RESULT_UNKNOWN`，不判定失败 |
| Redis 不可用且 DB 无订单 | 503 | 无法确认 |
| 订单号不存在 | 404 | `ORDER_NOT_FOUND` |

---

## 6. 库存、幂等与一致性保证

### 6.1 Redis 键与 Stream

```text
sk:stock:{itemID}                      库存余量（字符串整数）
sk:req:{userID}:{idempotencyKey}        请求状态 + payload 指纹 + 订单号（TTL 24h）
sk:stream:orders                      预约 Stream（MVP 不做 MAXLEN 裁剪）
sk:restocked:{orderNo}                 取消回补一次性标记（TTL 24h）
sk:rl:user:{userID}                     用户限流
sk:rl:global                            全局限流
```

Stream：

```text
group: order-workers
consumer: worker-{hostname}-{pid}
字段: idempotencyKey, userId, itemId, quantity, payloadHash, acceptedAt
```

MVP 不裁剪 Stream，避免误删未处理消息；增长风险列入已知限制。生产可改为“确认后裁剪/按活动保留”。

### 6.2 防超卖

1. **入口**：Redis Lua 条件扣减，库存不足直接拒绝，挡住绝大多数流量。
2. **落库**：MySQL 条件更新 `available_stock >= quantity`，事务内完成扣库存 + 限购 + 建单；这是最终保障。
3. **约束**：`available_stock` 不允许为负（UNSIGNED 或等价约束 + 条件更新）；订单数量与库存扣减在同一个本地事务中。
4. **Redis 可能漂移**（少卖或多放行）：
   - 少卖：不会超卖，但吞吐下降；
   - 多放行：MySQL 条件更新拒绝，不会生成超卖订单；
   - MVP 不做自动对账修复，差异靠告警与人工处理。

### 6.3 幂等保证

| 层 | 机制 | 作用 |
|---|---|---|
| HTTP | `Idempotency-Key` 必填，按用户隔离，记录 payload 指纹 | 拒绝不同 payload 复用 |
| Redis | Lua 中检查并写入请求状态 | 同键并发只放行一次预扣/一次入队 |
| Stream | 至少一次投递，允许重复消息 | 不承诺 exactly-once |
| MySQL | `UNIQUE(user_id, idempotency_key)` + 落库前回查 | 重复消息/重试只产生一个订单 |
| 限购 | `user_purchases` 唯一行 + 条件更新/行锁 | 同一用户总购买量不超过限购 |
| 取消 | 条件状态更新 + `UNIQUE(order_no, type)` | 只回补一次（DB） |
| 支付 | 条件状态更新 + `UNIQUE(order_no, provider_txn_id)` | 重复回调幂等 |

注意：Redis 幂等记录丢失后，同一幂等键可能被当作新请求再次预扣；但只要 MySQL 已有订单，Worker/查询仍按 `(user_id, idempotency_key)` 回查到同一订单。若 MySQL 也没有订单且 Stream 消息丢失，则该预约不可恢复，列入人工核查。

### 6.4 Redis 库存回补幂等

取消回补使用 Lua：

```text
if SET sk:restocked:{orderNo} NX EX ttl then
  INCRBY sk:stock:{itemID} quantity
end
```

Worker 因售罄/限购拒绝，或发现重复消息/已有订单时，使用按 Stream message ID 的独立补偿标记（例如 `sk:compensated:{messageID}`），避免重复 `INCRBY`。

限制：Redis 数据丢失会同时丢失标记与库存键，极端情况下可能重复回补或库存键缺失；MySQL 条件更新仍阻止超卖，但可能造成 Redis 少卖/多放行。MVP 不自动修复，需人工核查。

### 6.5 一致性结论

- **MySQL 是唯一可信结果**：订单、支付、取消、库存最终以 MySQL 为准。
- Redis 只影响“能否受理/多快受理”，不影响“是否超卖”。
- 不使用分布式事务，不承诺 Redis 与 MySQL 强一致。
- 不做自动对账/自动重建；出现差异时以告警和人工操作收尾。
- Redis Lua 的“原子”只保证执行期间不被其他命令插入，**不提供运行期错误回滚**。若脚本在扣库存后因 OOM、键类型冲突等异常中断，可能出现“已扣库存但消息/状态未写入”的半完成状态；实现必须做键类型前置检查、Redis `maxmemory-policy noeviction`、错误监控，并按 7.3 人工核查。该情况下 MySQL 条件更新仍可防止超卖，但不能假设 Redis 一定被正确扣减。

---

## 7. 故障边界与已知限制

### 7.1 依赖故障矩阵

| 故障 | 新订单 | 已受理请求 | 查询 | 恢复方式 |
|---|---|---|---|---|
| Redis 不可用 | `503`，不写 MySQL | Stream 消息与状态保留在 Redis（若未丢），Worker 无法拉取 | 有 DB 订单可查；否则 `503` | 恢复 Redis 后 Worker 继续；不自动重建/对账 |
| Redis 数据丢失 | 库存键缺失 → `503`，不自动初始化 | 可能丢失预约、幂等状态；DB 订单仍可查 | DB 有订单返回订单；否则 `RESULT_UNKNOWN` | 暂停入口，人工按 DB 重建活跃库存键并核查 |
| MySQL 不可用 | 健康检查/熔断 → `503`，不扣 Redis | 消息保留，Worker 不 ACK、退避重试 | 返回 `503` | 恢复后重试；不绕过 Redis 直写 |
| Worker 崩溃 | 入口仍可受理（依赖正常） | pending 消息由 `XAUTOCLAIM` 接管 | 可能短暂 `202` | 重启 Worker 自动重试（至少一次） |
| 取消调度崩溃 | 不影响 | 订单保持待支付 | 正常 | 重启后重新扫描；条件更新防重复 |
| 支付回调丢失 | 不影响 | 订单保持待支付，可能超时取消 | 用户可重试同一模拟支付 | 无自动对账 |
| 库存键缺失 | `503 NOT_READY` | 不处理新单 | 可查 DB | 人工/启动预热，不自动猜值 |

### 7.2 结果不明处理原则

出现下列任一情况，视为“结果不明”，**禁止自动回补或自动判失败**：

- HTTP 已返回/可能已返回 `202`，但 DB 无订单；
- Worker 提交事务超时、连接中断，无法确认提交结果；
- Redis 幂等状态为 `PROCESSING` 超过宽限期；
- Stream 消息在 pending 中多次投递仍未成功；
- Redis 数据丢失导致幂等键无记录。

处理方式：

1. 保留 Stream 消息，不 `XACK`；
2. 暂停相关商品/消息的自动处理，输出告警（日志 + 指标）；
3. 用同一幂等键查询 DB；
4. 按 7.3 人工核查后再决定重试、补单或补偿。

### 7.3 人工核查步骤

1. 暂停该商品的新入口流量与 Worker 自动处理（开关或停进程）。
2. 记录 `idempotencyKey`、`userId`、`itemId`、`quantity`、时间窗。
3. 查 MySQL：
   - `orders WHERE user_id=? AND idempotency_key=?`；
   - `orders WHERE user_id=? AND item_id=? ORDER BY id DESC`；
   - `stock_logs WHERE order_no=?`；
   - 商品 `available_stock` 与 `total_stock`。
4. 查 Redis：
   - `GET sk:req:{user}:{key}`；
   - `XRANGE sk:stream:orders ...` / `XPENDING sk:stream:orders order-workers`；
   - `GET sk:stock:{itemID}`；
   - 补偿/回补标记键。
5. 判定：
   - DB 有订单 → 以 DB 为准，更新 Redis 状态后恢复；
   - 消息仍 pending → 允许 Worker 重试（幂等键防重）；
   - 无订单、无消息、无幂等记录 → 标记 `RESULT_UNKNOWN`，由人工决定是否允许用户用同一幂等键重试；**不自动回补**；
   - 库存差异 → 人工以 DB 为准修正 Redis，并记录操作。
6. 恢复流量前验证：订单数、库存、流水一致；记录事故与处理人。

### 7.4 明确不做/不声称

- 不声称实现自动对账、自动库存修复、自动 Redis 重建、自动补偿闭环。
- 不声称 exactly-once；只承诺 DB 幂等下的“重复消息不重复建单”。
- 不声称 1000 RPS / 200 订单每秒已验证。
- 不实现真实退款：已取消订单的支付只记录冲突，人工处理。
- 不保证 Redis 故障期间受理结果不丢失。
- 不实现高可用、故障转移、跨机房。
- 活动时间窗校验可能使用 TTL 5～10 秒的 Redis 快照，且依赖应用时钟；边界附近可能有秒级误差。库存正确性仍由 MySQL 条件更新保障，但“未开始/已结束”的判定不是严格实时。
- 不做 Redis Stream 自动裁剪、积压自动清理或 DLQ 自动处理；内存与 pending 数量需人工监控。

---

## 8. 数据模型（实现阶段建表，本轮不执行）

> 以下为逻辑模型，最终字段/索引以实现时的迁移文件为准；本轮不提供或执行 DDL。

### 8.1 `activities`

- `id`、`name`、`start_at`、`end_at`、`status`、`created_at`、`updated_at`

### 8.2 `seckill_items`

- `id`、`activity_id`、`sku_code`、`name`、`price_cents`
- `total_stock`、`available_stock`（条件更新，不允许负）
- `per_user_limit`、`version`
- 索引：`(activity_id)`
- 约束：`available_stock >= 0`

### 8.3 `orders`

- `id`、`order_no`（唯一）、`user_id`、`item_id`、`activity_id`
- `quantity`、`amount_cents`
- `idempotency_key`、`payload_hash`
- `status`（`PENDING_PAYMENT` / `PAID` / `CANCELLED`）
- `expire_at`、`paid_at`、`created_at`、`updated_at`
- 唯一：`UNIQUE(user_id, idempotency_key)`、`UNIQUE(order_no)`
- 索引：`(status, expire_at)`、`(user_id, created_at)`

### 8.4 `user_purchases`

- `id`、`item_id`、`user_id`、`qty`、`created_at`、`updated_at`
- 唯一：`UNIQUE(item_id, user_id)`
- 用于限购与并发行锁

### 8.5 `stock_logs`

- `id`、`order_no`、`item_id`、`delta`、`type`（`DEDUCT` / `CANCEL`）、`created_at`
- 唯一：`UNIQUE(order_no, type)`
- 保证取消回补只记一次

### 8.6 `payments`

- `id`、`order_no`、`provider_txn_id`、`amount_cents`、`status`、`created_at`
- 唯一：`UNIQUE(order_no, provider_txn_id)`

### 8.7 Redis Stream

- 不建 MySQL 表；消息保留直到 Worker 成功提交并 `XACK`。
- MVP 不裁剪、不自动清理；运维需监控内存，人工清理前必须确认无未处理消息。

---

## 9. 最小验收用例（必须实现并验证）

### A1 并发不超卖

- 准备：库存 `M=100`，无历史订单；生成 `N=1000` 个不同用户、不同幂等键。
- 动作：并发压测入口（建议并发 200），收集 `202`/拒绝结果；等待 Worker 消费完。
- 断言：
  - MySQL `available_stock = 0`，且从未出现负值；
  - 有效订单数 `= 100`，订单总数不超过库存；
  - 支付前订单均为 `PENDING_PAYMENT`；
  - 不出现 500。
- 说明：Redis 预扣可能让部分请求直接 `SOLD_OUT`；最终以 DB 订单数与库存为准。

### A2 相同幂等键不重复下单

- 同一用户、同一幂等键、同一 payload 并发/重复请求 50 次。
- 断言：
  - 只产生 1 个订单；
  - 所有终态响应返回同一 `orderNo` 与状态；
  - 处理中请求返回 `202`，完成后返回 `200` 且结果一致；
  - 同一幂等键改 payload → `409 IDEMPOTENCY_CONFLICT`，不新增订单；
  - 压测中重复的 Stream 消息不新增订单。

### A3 订单超时取消回补一次

- 创建订单并使其过期（测试配置缩短 TTL）。
- 并发/重复执行取消调度两次以上。
- 断言：
  - 订单最终 `CANCELLED`，且只发生一次状态迁移；
  - MySQL `available_stock` 正好回补一次；
  - `stock_logs` 中 `(order_no, CANCEL)` 只有 1 条；
  - Redis 库存回补标记存在且只增加一次（Redis 可用时）。

### A4 Worker 重启后消息重试

- 在 Worker 拉取消息后、DB 提交前暂停/杀死 Worker（测试钩子或进程 kill）。
- 重启 Worker，等待 `XAUTOCLAIM` 接管 pending 消息。
- 断言：
  - 消息最终被处理，订单只创建一次；
  - 若“已提交但未 XACK”，重投消息被 DB 幂等键拦截，不新增订单；
  - 不因重启丢失已受理请求（前提：Redis 未丢数据）。

### A5 依赖不可用时的新订单行为

- **Redis 停止**：新下单请求在超时内返回 `503 SERVICE_UNAVAILABLE`；MySQL 订单数不增加；不存在绕过 Redis 的直接写库路径。
- **MySQL 停止**：新下单请求快速返回 `503`；Redis 库存不被扣减（或已扣减的请求保持未受理状态，不产生订单）；不绕过 Redis 写库。
- **结果不明**：构造已受理、Worker 未处理/Redis 状态丢失场景，按幂等键查询返回 `202` 或 `409 RESULT_UNKNOWN`，不得返回“失败”并自动回补；告警产生。
- **Redis 恢复后**：pending 消息可继续消费；不声称自动修复库存差异。

### A6 业务错误与查询语义

- 活动未开始、已结束、商品不存在、库存不足、限流：分别返回约定 4xx/429，不出现 500。
- 订单号不存在：`404 ORDER_NOT_FOUND`。
- 幂等键查不到但无法确认：`409 RESULT_UNKNOWN`。
- 重复模拟支付：返回同一支付结果；已取消订单支付：`409 PAY_AFTER_CANCEL`。

### A7 可重复性

- 单元测试覆盖：Lua 库存/幂等逻辑（或等价 fake）、Worker 幂等落库、取消回补幂等、错误映射。
- 集成测试覆盖 A1～A6（真实本地 Redis/MySQL，`SECKILL_IT=1` 开启）。
- `go test -race ./...` 通过；压测脚本可重复执行并自动校验关键断言。

---

## 10. 压测目标与说明

目标（未验证，仅作为设计与演示目标）：

- 入口 1000 请求/秒；
- Worker 200 新订单/秒；
- 不超卖、不重复建单。

说明：

- 这是目标值，不是承诺或已测结果；实现后需记录机器配置、Redis/MySQL 版本、并发数、成功率、P99、错误分布。
- 单 Redis + 单 MySQL + 单热点库存行是明确瓶颈；200 订单/秒同样对同一 SKU 的条件更新是压力点。
- 幂等查询（重复请求回查 DB）和订单状态轮询都会产生 MySQL 读；压测需包含轮询流量，不能只测入口。
- 限流阈值初始值（如每用户 5 RPS、全局 1000 RPS、Worker 并发 200）需通过 `dbprobe`/压测校准；README 只写“目标/初始配置”，不写“已验证”。
- Redis Stream 积压、内存增长、pending 数量必须监控；MVP 不做自动扩缩容。

---

## 11. 4～6 小时实施分配

| 时间 | 内容 | 产出 |
|---|---|---|
| 0:00–0:30 | 确认 SPEC、配置、迁移设计、接口骨架 | 文档冻结、环境变量清单 |
| 0:30–1:30 | MySQL 仓储/事务、Redis Lua、Stream 投递、202 接口 | 可受理请求 |
| 1:30–2:20 | Worker 消费、条件扣库存、限购、建单、幂等 | 异步下单闭环 |
| 2:20–3:00 | 订单查询、按幂等键查询、模拟支付 | 可演示主流程 |
| 3:00–3:40 | 超时取消、DB/Redis 回补幂等、人工告警路径 | 订单终态闭环 |
| 3:40–4:40 | A1～A5 测试：并发、幂等、重启、依赖故障、结果不明 | 自动化/脚本验证 |
| 4:40–5:30 | 压测脚本、指标/日志、README/API 同步 | 可演示报告（目标值标注） |
| 5:30–6:00 | 回归、`gofmt`/`go vet`/`go test -race`、风险复盘 | 可交付 MVP |

若时间不足，优先保证 A1、A2、A3、A4 与“依赖不可用快速失败”；压测规模、监控面板、复杂告警降级为后续。

---

## 12. 后续方案（明确未实现）

- 自动对账任务：MySQL 与 Redis 库存/流水定时比对与修复（需处理在途请求，不能盲目覆盖）。
- 自动 Redis 重建：基于 DB 快照与版本号/租约重建，需暂停流量或双写保护。
- DLQ 与自动重放：失败消息隔离、人工审批后重放。
- 支付对账与真实退款：支付流水下载、差异单、退款状态机。
- 库存分片/多行库存、热点行拆分；Kafka/更完整消息中间件。
- 多实例 Worker 协调、租约、限流分布式化、跨机房高可用。
- 完整鉴权、风控、防刷、审计与运营后台。
- 基于实测的容量规划与 SLO。

---

## 附录 A：原题（保留）

> 项目一：轻量级秒杀 / 限时抢购系统  
> 方向：电商核心 · 高并发交易
>
> 业务背景：社交电商 App 每场限时抢购有几百个 SKU，开场瞬间的请求量是日常的百倍以上。业务最怕两件事：库存超卖，以及系统被瞬时流量打垮。
>
> 这道题在考察什么：缓存与数据库的一致性、限流削峰、幂等设计。AI 很容易生成一段「看起来能跑」的扣库存代码，真正的分水岭是你有没有主动识别出并发下的超卖风险。
>
> 要实现的功能：
> - 活动与商品管理：活动时间窗、限购数量、总库存。
> - 抢购下单：校验活动状态与限购、扣减库存、生成订单。
> - 订单查询：用户能看到自己的订单及状态（待支付 / 已支付 / 已取消）。
> - 超时未支付自动取消并回补库存。
> - 一个最小的运营接口：创建活动、查询剩余库存。
>
> 设计中必须回答的问题：
> - 库存扣减发生在哪一层？缓存与数据库如何保持一致，出现不一致时怎么兜底？
> - 如何保证不超卖？同一用户重复点击或客户端重试时，如何保证幂等？
> - 限流和削峰的策略是什么？阈值依据是什么，而不是拍一个数字。
> - 扣减成功但下单失败、或支付回调丢失时，如何回滚或补偿？要不要对账？
> - 依赖（缓存、消息队列、数据库）不可用时如何降级？数据会退化成什么状态？
>
> 验收标准：
> - 用压测脚本模拟 N 个并发请求抢 M 件库存（N 远大于 M），结果不超卖，有效订单数不超过库存数。
> - 同一用户重复请求只产生一个有效订单，重复请求返回一致结果。
> - 活动未开始、已结束、库存不足时，接口返回明确的业务错误，而不是 500。
> - 单元测试覆盖并发扣减与幂等两类场景。
> - 服务重启后数据仍然正确，不依赖内存状态。
