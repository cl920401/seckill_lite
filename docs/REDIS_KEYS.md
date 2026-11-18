# Redis Key 与 Stream 设计

状态：MVP 设计，待实现  
事实来源：`docs/SPEC.md`；与本文档冲突时先修 SPEC。

---

## 1. 基本约定

- 单 Redis，不做 Cluster/分片；Key 前缀 `sk:` 可配置。
- `maxmemory-policy` 必须为 `noeviction`；库存、幂等、Stream 键不能被淘汰。
- 建议 AOF `everysec`，但只降低丢失窗口，不保证不丢数据。
- Key 仅由服务端拼接：`userID`、`itemID` 为十进制整数；`idempotencyKey` 已按 `^[A-Za-z0-9._~-]{8,128}$` 校验；`orderNo`、`reservationId` 为服务端生成的安全字符集。
- Lua 脚本放在 `internal/lua/*.lua`，通过 `go:embed` 加载。
- Lua 的“原子”只保证执行期间不被其他命令插入，**不提供运行期错误回滚**；脚本必须做类型检查，并监控半完成状态。

---

## 2. Key 清单

| Key | 类型 | 字段/值 | TTL | 说明 |
|---|---|---|---|---|
| `sk:stock:{itemId}` | string | 剩余库存整数 | 无 | 预扣/回补。缺失返回 `NOT_READY`，不自动创建 |
| `sk:req:{userId}:{idempotencyKey}` | hash | `status`、`reservation_id`、`payload_hash`、`order_no`、`reason`、`accepted_at`、`updated_at` | 24h | 入口幂等快速返回、Worker 状态回填。DB 仍是历史结果事实来源 |
| `sk:stream:orders` | stream | `reservationId`、`idempotencyKey`、`userId`、`itemId`、`quantity`、`payloadHash`、`acceptedAt` | 不裁剪 | 异步削峰队列 |
| `sk:restocked:{orderNo}` | string | `1` | 24h | 取消回补一次性标记；有限保护，见第 6 节 |
| `sk:compensated:{reservationId}` | string | `1` | 无 TTL，不主动清理 | 拒绝/额外预约补偿一次性标记 |
| `sk:rl:ingress:global` | hash | `tokens`、`ts` | 约 10s | 所有下单 POST（含幂等回放）的基础全局限流 |
| `sk:rl:query:global` | hash | `tokens`、`ts` | 约 10s | 查询接口限流，保护 MySQL 读 |
| `sk:rl:user:{userId}` | hash | `tokens`、`ts` | 约 10s | 仅新购买意图的每用户限流；幂等回放不消耗 |
| `sk:item:{itemId}` | hash | `activityId`、`startAt`、`endAt`、`activityStatus`、`priceCents`、`perUserLimit`、`totalStock`、`updatedAt` | 5～10s | 活动/商品快照；减少下单热路径 DB 读 |

### 2.1 `sk:req` 字段

```text
status          PROCESSING | SUCCEEDED | REJECTED
reservation_id  产生该状态的预约 ID
payload_hash    v1|itemId|quantity 的 SHA-256 十六进制
order_no        成功时写入，可空
reason          REJECTED 时的业务原因，例如 SOLD_OUT / LIMIT_EXCEEDED
accepted_at     受理时间，毫秒时间戳
updated_at      最近状态更新时间，毫秒时间戳
```

- 同一业务键的并发请求由 Lua 原子化：只允许一个 reservation 进入 Stream。
- 更新状态时必须校验 `reservation_id`，旧预约不得覆盖新结果。
- 24h TTL 到期不影响 DB 历史结果查询；入口 Redis miss 后回源 MySQL。

---

## 3. 限流

### 3.1 策略

| Key | 范围 | 初始配置（目标值，未验证） |
|---|---|---|
| `sk:rl:ingress:global` | 所有 `POST /api/v1/seckill/orders`，**包含幂等回放** | 1000 RPS，burst 1000 |
| `sk:rl:query:global` | 查询接口（幂等键查询、订单查询/列表） | 2000 RPS，burst 2000 |
| `sk:rl:user:{userId}` | 仅新购买意图 | 5 RPS，burst 10 |

- 所有下单请求先过 ingress 全局桶；被拒绝返回 `429 RATE_LIMITED`，不查 DB、不扣库存。
- 幂等回放不消耗 `sk:rl:user:{userId}`，但仍消耗 ingress 全局桶；因此 50 个同键并发请求允许部分 429。
- 未被限流的同键请求不得重复建单；后续重试返回同一历史结果。
- 查询接口先过 `sk:rl:query:global`，避免轮询无限打库。
- 阈值来自配置，需压测校准；当前只作为目标/初始值，不声明已验证。

### 3.2 Redis 不可用时

- 新订单：`503 SERVICE_UNAVAILABLE`，不进入 Lua，不写库。
- 历史结果：允许在本地并发/超时保护下回源 MySQL（如并发 32、200ms 超时），查到就返回；查不到则 503。
- 查询接口：Redis 限流不可用时使用本地并发保护，不得无限查库；这是降级，不是分布式限流。

---

## 4. Stream 与消费组

```text
stream: sk:stream:orders
group:  order-workers
consumer: worker-{hostname}-{pid}
```

### 4.1 生命周期

1. 启动时执行 `XGROUP CREATE sk:stream:orders order-workers 0 MKSTREAM`；`BUSYGROUP` 忽略。
2. `/readyz` 检查 Stream 与消费组；group 缺失时返回 503，Worker 告警，不自动重建业务库存。
3. 入口 Lua 中检查 `XLEN sk:stream:orders < STREAM_MAX_ENTRIES`（默认 10000）；超过返回 `QUEUE_FULL`，不扣库存。
4. Stream 不自动裁剪；上限包含已 ACK 条目，因此是生命周期容量上限，不是待处理数。
5. 达到上限后必须停入口，确认无 lag、无 pending、无在途事务/补偿，再人工安全清理已确认记录；MVP 不提供自动裁剪协议。

### 4.2 消费与 ACK

- `XREADGROUP GROUP order-workers CONSUMER worker-... COUNT n BLOCK ms STREAMS sk:stream:orders >`
- 成功订单：MySQL 事务提交成功后才 ACK；Redis 状态回填失败不影响 ACK，DB 可回源。
- 确定拒绝/额外预约：补偿 Lua 成功或已执行（`marker` 命中）→ DB `compensation_state=DONE` → 再 ACK。
- 补偿失败、库存键缺失、事务结果不明：保留 pending，不 ACK，退避重试/告警。
- 启动或周期执行 `XAUTOCLAIM` 接管 idle > 60s 的 pending；重复消息由 DB 幂等终态处理。
- 无自动 DLQ；达到重试阈值后保留 pending 并人工处理。

---

## 5. Lua 脚本契约

### 5.1 `reserve.lua`

- KEYS：`sk:stock:{itemId}`、`sk:req:{userId}:{key}`、`sk:stream:orders`
- ARGV：`userId`、`idempotencyKey`、`payloadHash`、`reservationId`、`itemId`、`quantity`、`acceptedAt`、`ttlMs`、`maxEntries`
- 返回码：
  - `ACCEPTED`：已扣 Redis 库存并写入 Stream/状态
  - `DUPLICATE`：同键已存在且指纹一致
  - `CONFLICT`：同键不同 payload
  - `SOLD_OUT`：库存不足
  - `NOT_READY`：库存键缺失/类型异常
  - `QUEUE_FULL`：Stream 达到上限
- 顺序：类型检查 → 幂等检查 → XLEN 检查 → `DECRBY` → `XADD` → `HSET req` + `PEXPIRE`。
- 脚本异常不回滚。若 `DECRBY` 后 `XADD`/`HSET` 失败，可能留下半完成状态；接口不得假装成功，必须告警并按人工步骤核查。DB 条件更新仍防超卖。

### 5.2 `mark_success.lua`

- KEYS：`sk:req:{userId}:{key}`
- 仅当 `HGET req reservation_id == reservationId` 时写 `status=SUCCEEDED`、`order_no`、`updated_at`。
- 旧预约或额外预约不得覆盖赢家结果；返回 `STALE` 时只记录，不影响 DB。

### 5.3 `compensate.lua`

- 调用前必须读持久预约终态：只有 outcome 为 REJECTED / EXTRA_RESERVATION 且 compensation_state=PENDING 才可补偿；DONE 直接 ACK，不因 Redis 标记缺失再次回补。同一成功预约也不补偿。
- 正常首次补偿没有 marker 是预期情况，不等于 Redis 数据丢失。若已知/已检测到 Redis 数据丢失或人工重置，则先暂停相关处理并人工核查，不直接重放 PENDING 补偿；MVP 不声称能自动识别所有静默数据丢失。
- KEYS：`sk:stock:{itemId}`、`sk:compensated:{reservationId}`、`sk:req:{userId}:{key}`
- 返回码：`OK`、`ALREADY`、`MISS`
- 库存键不存在：返回 `MISS`，**不创建库存键、不设置标记**。
- 标记已存在：返回 `ALREADY`，不再 `INCRBY`。
- 否则 `SET marker` → `INCRBY stock qty` → 条件更新请求缓存（仅当缓存 reservationId 匹配且状态不是 SUCCEEDED）。
- 标记无 TTL；由人工监控内存，不自动清理。

### 5.4 `restock.lua`

- KEYS：`sk:stock:{itemId}`、`sk:restocked:{orderNo}`
- 返回码：`OK`、`ALREADY`、`MISS`
- 库存键不存在：返回 `MISS`，不创建键、不设置标记。
- `SET sk:restocked:{orderNo} 1 NX EX 86400` 成功才 `INCRBY stock qty`。
- 标记只有 24h；过期后不得仅凭“标记不存在”再次回补，见第 6 节。

### 5.5 `rate_limit.lua`

- KEYS：对应限流 key
- ARGV：`rate`、`burst`、`nowMs`、`cost`、`ttlMs`
- 返回：`ALLOWED` / `LIMITED`、剩余令牌、建议重试毫秒。
- 令牌桶；Key 首次使用时按 burst 初始化。

---

## 6. `sk:restocked` 的有限保证

- 24h TTL 仅覆盖正常自动重试窗口，不承诺任意时间重复回补安全。
- 自动取消只处理 DB 中 `PENDING_PAYMENT` 且过期的订单；首次取消提交后状态为 `CANCELLED`，调度不会再次回补。
- 标记过期不等于需要回补；人工补偿前必须：
  1. 暂停相关商品入口、Worker 与取消任务；
  2. 查 DB 订单状态为 `CANCELLED`；
  3. 查 `stock_logs(order_no, 'CANCEL')` 已存在、DB `available_stock` 已回补；
  4. 查 Redis 库存键、标记、在途 pending 预约；
  5. 确认没有仍会落库或回补的在途事务后，才人工修正 Redis；
  6. 记录操作人、时间、前后值。
- 任何情况下不得仅凭“查不到标记/查不到订单”执行 `INCRBY`。

---

## 7. 故障与就绪

| 场景 | 行为 |
|---|---|
| 库存键缺失 | 下单 `503 NOT_READY`；不自动初始化 |
| Stream/group 缺失 | `/readyz` 503；新单依赖库存键与 Redis 可用性，不自动重建 |
| Redis 不可用 | 新单 503；历史结果在本地有界保护下回源 MySQL；查询可降级 |
| Redis 数据丢失 | 可能丢失未落库预约、幂等状态、Stream、补偿标记；人工核查，不自动重建 |
| Stream 满 | 429 `QUEUE_FULL`，不扣库存；人工清理 |
| 正常首次补偿，marker 尚不存在 | DB 拒绝/额外预约终态已提交且补偿 PENDING，按 compensate.lua 原子执行；不能仅凭 marker 缺失判定数据丢失 |
| DB 补偿已 DONE，marker 不存在 | 直接 ACK，不再回补 |
| 已知/检测到 Redis 数据丢失或人工重置，补偿仍 PENDING | 暂停相关处理、保留消息、告警并人工核查；不能按普通首次补偿重放 |

不声称实现自动对账、自动库存修复、自动 Redis 重建或完整高可用。

