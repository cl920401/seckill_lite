# 数据库结构设计

状态：MVP 已实现；运行库 DDL 未执行，seckill_test 测试库已执行
DDL：`migrations/0001_init.sql`（运行库尚未执行）
事实来源：`docs/SPEC.md`；与本文档冲突时先修 SPEC。

---

## 1. 设计原则

- MySQL 是订单、库存、限购、支付、取消的唯一事实来源。
- InnoDB；默认隔离级别 `REPEATABLE READ`。
- 默认字符集 `utf8mb4`；幂等键、订单号、预约号、payload 哈希使用 `ascii_bin`，保证大小写敏感和精确比较。
- 时间使用 `DATETIME(3)`，应用写入 UTC。
- 不建外键：热路径避免额外检查与锁竞争；完整性由事务、唯一键、条件更新和测试保证。
- 不加 ORM 隐式行为；SQL 集中在 `internal/repo/mysql`。
- 不编辑已有迁移；新增迁移按编号追加。当前只有 `0001_init.sql`，未执行。

---

## 2. 表概览

| 表 | 作用 | 关键约束 |
|---|---|---|
| `activities` | 活动时间窗与状态 | `end_at > start_at` |
| `seckill_items` | 商品、总库存、可售库存、限购 | `available_stock <= total_stock`；`per_user_limit > 0` |
| `orders` | 订单与状态 | 唯一 `order_no`、`(user_id, idempotency_key)`、`reservation_id` |
| `request_results` | 业务键（用户+幂等键）持久终态 | 唯一 `(user_id, idempotency_key)` |
| `reservation_results` | 拒绝/额外预约的持久终态与补偿进度 | 唯一 `reservation_id` |
| `user_purchases` | 用户对单品的累计购买量 | 唯一 `(item_id, user_id)` |
| `stock_logs` | DB 库存扣减/回补流水 | 唯一 `(order_no, type)` |
| `payments` | 模拟支付流水 | 唯一 `(order_no, provider_txn_id)` |

### 2.1 状态值

```text
activities.status: ACTIVE | DISABLED

orders.status: PENDING_PAYMENT | PAID | CANCELLED
PAID / CANCELLED 为终态。

request_results.state: PROCESSING | SUCCEEDED | REJECTED
PROCESSING 只在 Worker 事务内短暂存在；提交后必须是终态。
REJECTED 是业务键的稳定终态，新的购买意图必须换幂等键。

reservation_results.outcome: REJECTED | EXTRA_RESERVATION
reservation_results.compensation_state: PENDING | DONE

stock_logs.type: DEDUCT（delta < 0）| CANCEL（delta > 0）
payments.status: SUCCESS（MVP 仅模拟成功）
```

---

## 3. 关键唯一键与索引

### 3.1 幂等与防重

- `orders.UNIQUE(user_id, idempotency_key)`：同键最多一单。
- `orders.UNIQUE(reservation_id)`：同一成功预约最多一单。
- `request_results.UNIQUE(user_id, idempotency_key)`：所有 Worker 对同一业务键先锁这一行。
- `reservation_results.UNIQUE(reservation_id)`：一个预约的拒绝/额外终态只记一次。
- `payments.UNIQUE(order_no, provider_txn_id)`：支付回调幂等。
- `stock_logs.UNIQUE(order_no, type)`：取消回补只记一次。

### 3.2 查询/任务索引

- `orders(status, expire_at, id)`：超时取消扫描。
- `orders(user_id, id)`：用户订单列表（`beforeId` 游标）。
- `orders(item_id, status)`：排查单品订单。
- `request_results(order_no)`、`request_results(winning_reservation_id)`：回查。
- `reservation_results(compensation_state, outcome, id)`：补偿告警与人工处理。
- `stock_logs(item_id, created_at)`：库存流水排查。
- `user_purchases(item_id, user_id)`：限购行锁。

---

## 4. Worker 建单事务协议

所有 Worker 对同一业务键遵循同一顺序：

```text
BEGIN
1. 锁定 request_results 业务键行
2. 判断请求/预约终态
3. 无终态时：条件扣库存
4. 锁定并更新 user_purchases，校验限购
5. INSERT orders + stock_logs(DEDUCT)
6. 写 request_results SUCCEEDED + winning_reservation_id
COMMIT
```

### 4.1 锁定请求行

```sql
INSERT INTO request_results
  (user_id, idempotency_key, payload_hash, winning_reservation_id, state)
VALUES (?, ?, ?, ?, 'PROCESSING')
ON DUPLICATE KEY UPDATE id = id;

SELECT *
  FROM request_results
 WHERE user_id = ? AND idempotency_key = ?
 FOR UPDATE;
```

- 不使用 `INSERT IGNORE`，避免吞掉非重复错误（例如字段超长、非法值）。
- 若 `payload_hash` 不一致：不覆盖已有结果，本次预约终结，进入补偿/告警。
- 若已有 `SUCCEEDED`：同一 `winning_reservation_id` 的重投只回填结果并 ACK，不扣库存；不同 reservationId 进入 `EXTRA_RESERVATION` 补偿。
- 若已有 `REJECTED`：不得再建单；不同 reservationId 同样进入额外预约补偿。

### 4.2 条件扣库存

```sql
UPDATE seckill_items
   SET available_stock = available_stock - ?,
       version = version + 1
 WHERE id = ? AND available_stock >= ?;
```

- 影响行数为 0 = 售罄/库存不足，回滚业务变更。
- `available_stock` 为 `UNSIGNED`，并由 `CHECK (available_stock <= total_stock)` 约束；条件更新保证不会为负。
- 不要用 `SELECT ... FOR UPDATE` 先读后扣替代条件更新；条件更新是最终防超卖保障。

### 4.3 限购

先确保计数行以 `qty=0` 存在，再对首次及后续购买统一执行限购条件更新；不得在 UPDATE 影响 0 行后直接插入购买数量：

```sql
INSERT INTO user_purchases (item_id, user_id, qty)
VALUES (?, ?, 0)
ON DUPLICATE KEY UPDATE id = id;

UPDATE user_purchases
   SET qty = qty + ?
 WHERE item_id = ? AND user_id = ? AND qty + ? <= ?;
-- quantity 必须为正整数；最后一个参数为该商品的 per_user_limit。
-- UPDATE 影响 0 行即 LIMIT_EXCEEDED；不能改走无条件插入/累加。
```

- 必须在事务内、持有 `request_results` 行锁后执行。`user_purchases(item_id,user_id)` 唯一行上的写锁使不同幂等键的并发购买也串行检查限购。
- SAVEPOINT 在请求行锁取得后、库存扣减与计数建行之前建立。限购失败时 `ROLLBACK TO SAVEPOINT` 撤销库存/限购变更，保留请求行锁，再写 `request_results REJECTED` 与预约补偿终态后提交。
- 验收至少覆盖：首次 quantity 大于 limit 被拒绝、首次恰好达到 limit 成功、后续超限被拒绝、同用户不同键并发总量不超过 limit。

- 取消订单时递减 `qty`，保留行，不删除。

### 4.4 建单与流水

```sql
INSERT INTO orders (..., status, expire_at)
VALUES (..., 'PENDING_PAYMENT', ?);

INSERT INTO stock_logs (order_no, item_id, delta, type)
VALUES (?, ?, -?, 'DEDUCT');
```

- `orders.amount_cents` 由服务端按商品价格 × 数量计算，不信任客户端金额。
- `expire_at` 由应用生成；取消扫描使用数据库 `NOW(3)` 或统一应用时间，二者不能混用。

### 4.5 锁顺序

- Worker：`request_results` → `seckill_items` → `user_purchases` → `orders` / `stock_logs`。
- 取消：`orders` → `seckill_items` → `user_purchases` → `stock_logs`。
- 支付：仅 `orders`（+ `payments`）。
- 所有事务必须有超时与重试上限；死锁按指数退避有限重试，不得无界重试。

---

## 5. 取消事务

```text
BEGIN
1. UPDATE orders SET status='CANCELLED', cancelled_at=NOW(3)
    WHERE id=? AND status='PENDING_PAYMENT';
   影响 0 行 -> 已被支付/取消，ROLLBACK/跳过
2. UPDATE seckill_items
      SET available_stock=available_stock+?, version=version+1
    WHERE id=?;
3. UPDATE user_purchases SET qty=qty-? WHERE item_id=? AND user_id=?;
4. INSERT stock_logs(order_no, item_id, +?, 'CANCEL');
COMMIT
```

- 条件状态更新保证与支付竞争时只有一个成功。
- `UNIQUE(order_no, 'CANCEL')` 保证重复调度不会重复回补 DB。
- 提交后再执行 Redis 回补 Lua；Redis 失败不自动修复，保留告警/人工处理。

---

## 6. Redis 回补标记的有限性

- `sk:restocked:{orderNo}` 为 24h TTL，只覆盖短期自动重试窗口，**不是永久幂等保证**。
- 自动取消只针对 DB 状态为 `PENDING_PAYMENT` 且过期的订单；首次取消提交后订单已是 `CANCELLED`，正常调度不会再次回补。
- 标记过期后不得因为“标记不存在”就再次 `INCRBY`。人工补偿前必须：
  1. 暂停该商品入口与 Worker/取消任务；
  2. 确认 DB 订单已是 `CANCELLED`；
  3. 确认 `stock_logs(order_no,'CANCEL')` 已存在且 DB `available_stock` 已回补；
  4. 检查 Redis 库存键、标记与在途预约；
  5. 人工决定是否修正 Redis，并记录操作人、时间、前后值。
- 不承诺任意时间点的重复回补都安全；Redis 数据丢失后更需人工核查。

---

## 7. 迁移与执行边界

- 当前仅新增 `migrations/0001_init.sql`，未执行任何 SQL。
- 执行迁移、写库、清库、压测前必须获得用户明确确认。
- 不修改已有迁移；变更追加新编号。
- 表结构变更必须同步 `docs/SPEC.md` 第 8 节与本文档。

