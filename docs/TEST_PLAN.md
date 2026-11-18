# 秒杀 MVP 测试规范（TEST PLAN）

> 事实来源：`docs/SPEC.md`。本文件只定义“测什么、测到哪、怎么证”，不修改业务语义。
> 关联：`docs/API.md`、`docs/SCHEMA.md`、`docs/REDIS_KEYS.md`、`migrations/0001_init.sql`、README、附录 A 原题。
>
> **执行状态声明（重要）**
> - 本文件**不代表任何集成/压测“已经通过”**；**“测试存在” ≠ “测试通过”**。
> - 本轮只做了静态检查与单元层验证；**未运行任何写 MySQL/Redis 的测试**。
> - 集成、Lua 契约、压测必须由执行者在独立测试环境运行并填写第 10 节记录后才可标记通过。
> - 未执行项（真实停服、杀进程、24h TTL、RPS）必须在记录中显式标注。

---

## 0. 结论速览

- MVP 硬承诺：**不超卖、同键不重复建单、取消回补一次、重复消息不重复建单、预扣前已识别依赖故障快速失败、结果不明不自动回补**。
- 高并发“全都成功”、跨系统强一致、自动对账/修复、exactly-once、真实高可用、已验证 1000 RPS 均**不是承诺**。
- P0（4～6 小时必测）：不超卖、同键幂等、取消回补、消息重投/补偿、依赖故障安全行为。
- P1（有余力）：真实停服/杀进程、完整指标、大规模压测、HTTP 真栈、并发认领、限流细节。
- 边界内不一致：**Fix-3、Fix-7、Fix-8 已修复并验证**（R2/最终轮）；仍待处理：Fix-2（P1）、Fix-4（P2）。已知限制：Redis 回补不自动重试、无指标平台（见第 5 节）。
- 针对性补充用例：T1～T5（见第 6 节）；T3 随 Fix-7 修复通过；最终轮补充结果不明与补偿部分执行验证。
- 最终轮（2026-11-19）在 `seckill_test` + Redis db14 执行：单元/Lua 通过，集成 **25/25** 通过，最小演示通过。

---

## 1. 测试边界

### 1.1 MVP 已承诺的保证（必须验证）

| 编号 | 保证 | 依据（SPEC） |
|---|---|---|
| G1 | MySQL 同一事务内条件扣减，库存不为负，订单数不超过库存 | 6.2、8.2、9/A1 |
| G2 | 同一 `(user_id, idempotency_key)` + 同 payload 只产生一个订单；不同 payload 冲突 | 4.2、6.3、9/A2 |
| G3 | `202` 仅表示已受理；查询区分 `PROCESSING`/订单/`REJECTED`/`RESULT_UNKNOWN`/依赖不可用 | 4.2、4.4、5.5 |
| G4 | 取消只针对已落库的过期 `PENDING_PAYMENT`；DB 回补一次；Redis 可用时回补一次 | 4.6、6.4、9/A3 |
| G5 | 支付幂等；已取消订单支付返回 `PAY_AFTER_CANCEL`，不退款 | 4.5、5.4 |
| G6a | **预扣前已识别** Redis/MySQL 不可用：新订单快速失败（503），不执行 Lua、不扣 Redis、不绕过 Redis 写库 | 4.2(5)、7.1、9/A5 |
| G6b | **预扣入队后**依赖故障：保留已受理事实，不自动判失败/回补；恢复后按原幂等键查询 | 4.2(5)、7.1、7.2 |
| G7 | Stream 至少一次投递下，重复消息不重复建单/不重复回补 | 4.3、6.3、9/A4 |
| G8 | 结果不明时保留消息、不自动回补、不判失败，人工核查 | 7.2、7.3 |
| G9 | 重启保留已有 Redis 库存，不用 DB 覆盖在途预扣；缺失保持未就绪 | 4.1(5)、9/A2 |
| G10 | 全局限流先于幂等查询；回放可 429 但不重复建单；回放不消耗每用户新单令牌 | 4.2、6.1、9/A7 |
| G11 | 业务分支不返回 500；错误码与 HTTP 状态符合 5.3 | 5.3、9/A6 |

**关于“预扣前 MySQL 检查”的澄清**：下单流程在读取商品缓存**之前**，必然先执行 MySQL 幂等查询（`service.go` 的 `lookupHistoryGuarded` → `store.LookupRequest`）。因此：
- MySQL 在预扣前不可用 → 该查询失败 → `503`，不会进入 `loadItem`/Lua/Redis 预扣。这不是“仅因缓存命中而缺少依赖检查”。
- 检查通过后 MySQL 才发生故障 → 属于 **G6b 结果不明**，不要求“无副作用”。

### 1.2 只要求“安全失败 + 保留证据 + 人工处理”

以下场景不承诺自动正确，只验证约定的安全行为：**不自动回补、不自动判失败、不绕过依赖、保留消息/证据、可解释状态**。

| 编号 | 场景 | 安全行为要求 | 依据 |
|---|---|---|---|
| S1 | Redis 静默数据丢失/重置 | 库存键缺失→503；DB 订单可查；无自动重建/回补 | 4.1、7.1、7.4 |
| S2 | 依赖检查通过后 MySQL 故障 | 不撤销已受理事实；保留消息；恢复后按原键查询；不自动补偿 | 7.1、7.2 |
| S3 | Worker 提交结果不明（超时/断连） | 不 ACK、不补偿；重取锁后读持久结果；无法确认保留 pending | 4.3(3)、7.2 |
| S4 | `RESULT_UNKNOWN` | 409，不据此判失败或回补 | 4.4、5.5 |
| S5 | `sk:restocked` 过期/缺失 | 不得仅凭标记缺失再次 `INCRBY`；人工核对 DB/CANCEL 流水/库存 | 6.4 |
| S6 | 补偿成功但 DB DONE 未写 | 重投命中 `sk:compensated` 标记后补写 DONE，只补偿一次 | 4.3(5)、6.4 |
| S7 | 取消回补时 Redis 不可用 | DB 已回补；仅告警；Redis 少卖需人工处理；**不要求新增自动重试/恢复** | 4.6(3) |

### 1.3 明确不在范围（不做、不测、不声称）

- 自动对账、自动库存修复、自动 Redis 重建、自动补偿闭环、自动重试/恢复机制。
- 死信系统/DLQ 自动重放；复杂补偿工作流引擎。
- 真实支付网关、真实退款、支付对账文件。
- 高可用、故障转移、跨机房、多活、Redis Cluster、MySQL 主从、Kafka、库存分片。
- exactly-once；仅承诺“重复消息不重复建单”。
- 已验证的 1000 入口 RPS / 200 订单每秒（仅目标值）。
- Stream 自动裁剪、完整鉴权、风控、防刷、运营后台、生产监控平台。
- 多租户与生产级 SLO。

### 1.4 边界判定原则

| 情况 | 必须断言 | 不应断言 |
|---|---|---|
| 预扣前依赖故障 | 503、无预扣、无 DB 写、无 Stream 消息 | “恢复后自动补齐” |
| 预扣后依赖故障 | 已受理事实保留、消息保留、恢复后同键可得结果 | 无副作用、必然落单、必然失败 |
| 结果不明 | 409/503、消息保留、不自动回补 | “一定失败/一定回补” |
| Redis 漂移 | DB 最终不超卖 | Redis 与 DB 强一致 |
| 重复消息 | 不重复建单/不重复回补 | exactly-once |
| 限流 | 429、无库存副作用、窗口后结果一致 | 并发重试全部成功 |
| 标记过期 | 不以标记缺失为回补依据 | 任意时刻重复回补安全 |

---

## 2. 测试分层、数据隔离与运行边界

| 层 | 位置 | 依赖 | 触发 | 职责 |
|---|---|---|---|---|
| 单元 | `internal/httpapi`、`internal/service`、`internal/worker` | 无（fake） | 默认 `go test -race ./...` | 校验、错误映射、限流顺序、状态机、Worker 决策与告警日志 |
| Lua 契约 | `internal/redisrepo/gateway_test.go` | 真实 Redis | `SECKILL_REDIS_IT=1` | 5 个脚本返回码/原子性/标记语义 |
| 集成 | `internal/integration` | 真实 MySQL + Redis | `SECKILL_IT=1` | A1～A7 主流程、幂等、终态、补偿/重投窗口 |
| 压测 | `cmd/loadtest` | 真实全栈 | 手动 | 目标值观测 + 只读校验，不作为 CI 断言 |

### 2.1 强制隔离规则

1. **专用测试库**：集成库名必须以 `_test` 结尾（如 `seckill_test`），且 `SECKILL_IT_DSN` 必须显式设置。已在 `setup` 中加守卫：缺省、无法解析或库名不以 `_test` 结尾时，在建立连接前 `t.Fatal`，避免误写运行库。
2. **串行执行**：集成包内不使用 `t.Parallel()`；建议命令加 `-p 1`，同一 `*_test` 库同一时刻只允许一个集成测试进程。
3. **取消扫描不可只靠“只断言本单”**：`CancelExpired` 全局扫描 `PENDING_PAYMENT AND expire_at <= now`，会扫到其他用例/历史运行的订单。因此取消相关用例在创建被测订单**之前**调用 `drainExpiredOrders`，把库中既有过期订单清空；再断言本单库存/流水/批次条数。该清理只发生在 `*_test` 库。
4. **Redis 隔离**：每个用例唯一前缀 + `t.Cleanup` 删除本前缀键；断言只针对本前缀/本 item。
5. **禁止生产数据**：不得使用生产 DSN、生产 Redis、真实支付、真实个人信息、真实密钥；`.env` 不入库。
6. **不写运行库、不自动建表**：执行迁移/压测写库/清库前必须确认；`*_test` 库需由执行者预先执行 DDL。
7. **可重复运行**：不依赖测试顺序；不使用真实 24h 等待（用删标记/缩短配置模拟）；重启/重投用消息与持久终态驱动。

### 2.2 P0 / P1（4～6 小时约束）

- **P0**：不超卖、同键幂等、取消回补、消息重投/补偿、依赖故障安全行为（预扣前 503 / 预扣后结果不明）、基础业务错误。
- **P1**：真实停服、真实杀进程、完整指标、大规模压测、HTTP 真栈 e2e、并发 `XAUTOCLAIM`、查询限流、支付/取消错误码、缓存权威性修复后的用例。
- 时间不足时压缩 P1；不得删减 P0 的幂等/重投正确性。

---

## 3. 场景矩阵

记号：`P`=优先级；“现有用例”只表示代码存在，**未执行**；`部分/缺失/无法验证` 为覆盖状态。

### 3.1 正常流程

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| N1 | P0 | `*_test` 库已建表；Redis 可用 | 创建活动+商品 | `201`；MySQL `available_stock=total_stock`；Redis `sk:stock` 存在；`redisReady=true` | HTTP + 查 MySQL/Redis | 部分（service 单测 fake；集成 `createItem` 未断言 201） |
| N2 | P0 | N1；限流额度充足；依赖健康 | 新用户/新键下单 | `202` + `requestId`/`idempotencyKey`/`PROCESSING`/轮询地址 | HTTP + Stream 字段 | 部分（`TestServiceEndToEndFlow` 走 service） |
| N3 | P0 | N2 | Worker 处理 | 一条 `PENDING_PAYMENT`；库存 -1；`request_results=SUCCEEDED`；`DEDUCT` 流水 1 | 查 MySQL | 存在未执行 |
| N4 | P0 | N3 | 按幂等键/订单号/列表查询 | `200`；列表仅当前用户；分页正确 | HTTP | 存在未执行 |
| N5 | P0 | N3 | 模拟支付；同 txn 重复回调 | 首次 `PAID`；重复幂等 `PAID`；`payments` 唯一 | HTTP + MySQL | 存在未执行（`TestPaymentIdempotentAndAfterCancel`） |
| N6 | P0 | N3，缩短过期；先 drain | 触发取消调度 | `CANCELLED`；DB/Redis 各 +qty 一次；`CANCEL` 流水 1 | MySQL + Redis + 流水 | 存在未执行（`TestA3CancelRestockOnce`） |
| N7 | P1 | N1 | 查活动库存（`includeRedis=true`） | MySQL/Redis 分别返回；异常给 warning | HTTP + Redis | 部分（单测 fake） |

### 3.2 并发与库存

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| C1 | **P0** | 库存 `M=100`；无历史订单；`N=1000` 不同用户/键；**限流额度足够高不干扰**、HTTP 超时足够、Redis/MySQL 健康、Worker 已启动且允许排空 | 并发入口（目标并发 200），等待消费完成 | DB `available_stock=0` 且从不为负；有效订单=100；总数≤库存；支付前均 `PENDING_PAYMENT`；无 500；**恰好售罄**且不超卖 | 压测脚本 + `verify.sql` 只读校验 | 部分（`TestA1ConcurrentNoOversell` 为 repo 层，非 HTTP） |
| C2 | **P0** | 库存 1；人为让 Redis 多放行 1；限流/超时/依赖健康 | 两个不同用户/键都过 Redis 预扣 | MySQL 只成功 1 单；第二个进入 `EXTRA/REJECTED` 补偿；DB 库存 0 | 查订单/库存/补偿终态 | 存在未执行（`TestDBFinalGuardAgainstRedisOverAdmit`） |
| C3 | P1 | Redis 少放行（人为少扣） | 发起 M 个请求 | 订单数 `<M`，但不超卖；不自动修复差异 | 查订单/库存 | 缺失（安全行为，无需修复） |
| C4 | P1 | `per_user_limit=2` | 同一用户多键并发购买 | `user_purchases.qty` 累计≤2；超限单被拒 | 订单/限购表 | 部分（顺序用例） |
| C5 | P1 | 活动开始/结束边界 | 边界附近下单 | 允许秒级误差；不超卖；无 500 | 时间窗配置 + MySQL | 缺失（已知限制） |

### 3.3 幂等

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| I1 | **P0** | 同用户/键/同 payload；限流/超时/依赖健康 | 并发 50 次 + Worker 处理 | 允许部分 429；未限流请求不重复建单；DB 仅 1 单；`request_results` 1 行；无第二个 `orderNo`；无 500 | 并发脚本 + MySQL | 部分（`TestA2SameKeyOneOrder` + 单测；无 HTTP 429 断言） |
| I2 | **P0** | 已有成功订单 | 重投同一 reservation | 不新增订单；Redis 库存不增加 | 重投 + 查 | 存在未执行（`TestA4WorkerReclaimsPending`） |
| I3 | **P0** | 已有键 | 同键改 payload | `409 IDEMPOTENCY_CONFLICT`；不新增订单 | HTTP/service | 存在未执行（单测 + Lua 契约） |
| I4 | P1 | 已完成订单 | 等限流窗口后同键顺序重试 | 返回同一历史结果 | HTTP | 缺失 |
| I5 | **P0** | 已有订单 | 删除 Redis 幂等键；模拟活动结束/售罄后重试 | 以 MySQL 历史订单为准，`200` | 删键 + 查 MySQL | 部分（`TestA2ReplayFromDBAfterRedisKeyDeleted`） |
| I6 | **P0** | 同业务键两个不同 reservationId | 两个预扣都到 Worker | 只 1 单；第二个 `EXTRA_RESERVATION`；补偿一次 | 订单/预约终态/Redis | 存在未执行（`TestExtraReservationCompensatedOnce`） |
| I7 | P1 | 限流配置较小 | 新意图后立即回放 | 回放可 429；不消耗用户新单令牌 | HTTP + Redis | 部分（`TestA7GlobalRateLimitAppliesToReplay` + 单测） |
| I8 | **P0** | 已预扣未落库 | 模拟重启初始化（`SET NX`/`EnsureGroup`） | 不覆盖 Redis 库存 | Redis | 存在未执行（`TestA2RestartDoesNotOverwriteInFlightReservation`） |

### 3.4 超时取消与回补

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| X1 | **P0** | 专用 `*_test` 库；串行；先 `drainExpiredOrders`；订单过期 | 顺序/重复执行取消调度两次以上 | 只一次 `PENDING_PAYMENT→CANCELLED`；DB 库存 +qty 一次；`CANCEL` 流水 1；批次含且仅含本单 | 订单/库存/流水/批次 | 部分（`TestA3CancelRestockOnce`；并发缺失） |
| X2 | **P0** | Redis 可用 | 取消后检查标记 | `sk:restocked:{orderNo}` 存在；库存只 +qty 一次 | Redis + 库存 | 存在未执行（`TestA3RestockMarkerMissingDoesNotDoubleRestock`） |
| X3 | **P0** | 已取消 | 删除/过期 `sk:restocked` 后再调度/重放 | 不再 `INCRBY`；订单仍 `CANCELLED`；流水仍 1 | 删键 + 调度 + Redis | 存在未执行（同上） |
| X4 | **P0** | 已取消订单 | 模拟支付 | `409 PAY_AFTER_CANCEL`；不退款 | HTTP/service | 存在未执行（`TestPaymentIdempotentAndAfterCancel`） |
| X5 | P1 | 订单刚过期 | 并发支付与取消 | 终态只能 `PAID`（不补库存/无 CANCEL 流水）或 `CANCELLED`（库存+1/流水1） | 并发 + 终态 | 部分（仅终态，未断言错误码） |

### 3.5 Worker 重启与消息重试

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| W1 | **P0** | 消息已读未 ACK | `XAUTOCLAIM` 接管后处理 | 订单只创建一次 | ReadGroup→Process→Claim→Process | 存在未执行（`TestA4WorkerReclaimsPending`） |
| W2 | **P0** | 已提交未 ACK | 同预约重投 | 不新增订单；Redis 库存不增加 | 重投 + 查 | 存在未执行（`TestA4RedeliveryAfterCommitDoesNotRestockRedis`） |
| W3 | **P0** | 已拒绝、Redis 已补偿、DB 未写 DONE | 重投 | 命中 `sk:compensated`；补写 DONE；只补偿一次 | 手动补偿后重投 | 存在未执行（`TestA4CompensationCrashWindows`） |
| W4 | **P0** | `compensation_state=DONE` | 删除 marker 后重投 | 只 ACK，不再次回补 | 删键 + 重投 | 存在未执行（`TestA4CompensationCrashWindows` + `TestProcessDONEOnlyAcks`） |
| W5 | P1 | 两个 Worker 同时认领同一 pending | 并发 `XAUTOCLAIM` | 持久终态阻止重复建单/补偿 | 双消费者 | 缺失 |
| W6 | P1 | 真实进程 | `SIGKILL` 后重启 | 重启接管 pending；不丢已受理请求 | 进程脚本 | **无法验证**（人工） |
| W7 | **P0** | 提交超时/断连 | 测试替身在真实提交后返回结果不明，再重试 | 首次不 ACK/不补偿；重试同预约得到唯一订单并 ACK；订单数不增、Redis 不增 | 测试替身 + 真实 DB/Redis | 已覆盖：`TestProcessUnknownCommitKeepsPendingThenUniqueResult`、`TestUnknownCommitResultRecoversUnique` PASS |

### 3.6 依赖故障（区分预扣前/后）

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| D1 | **P0** | Redis 不可用；限流/超时已配置 | 新用户/新键下单 | `503`；MySQL 无新增订单；无绕过路径 | 无效 Redis 地址/停 Redis | 存在未执行（`TestA5RedisUnavailableNewOrderFails`，注入式） |
| D2 | **P0** | MySQL 在**预扣前**不可用 | 新用户/新键下单 | `503`；不执行 Lua；Redis 库存不变；不写库 | 注入 store 错误/停 MySQL | 存在未执行（`TestA5MySQLUnavailableBeforeReserve`，注入式） |
| D3 | P1 | 检查通过后 MySQL 故障 | Lua 入队后停 MySQL | 保留已受理事实；消息保留；**不要求无副作用**；恢复后同键得到唯一订单或持久拒绝；同预约重投不重复扣减/回补 | 测试钩子 | 部分：提交结果不明已用测试替身覆盖（W7）；真实停 MySQL 未执行 |
| D4 | **P0** | 已受理、Worker 未处理 | 删除 Redis `sk:req` 后查询 | `409 RESULT_UNKNOWN`；不自动回补；库存不变 | 删键 + 查询 | 存在未执行（`TestA5ResultUnknownNotCompensated`） |
| D5 | **P0** | DB 已有订单、Redis 不可用 | 同键重放 | `200` 历史订单 | 无效 Redis 地址 | 存在未执行（`TestA5HistoryReturnedWhenRedisUnavailable`） |
| D6 | P1 | Redis 恢复 | 观察 pending 消费 | 继续消费；不声称自动修复库存差异 | 恢复后观察 | 缺失/人工 |
| D7 | **P0** | 库存键缺失 | 下单 | `503 NOT_READY`；不初始化、不扣减 | 删库存键（Lua 层） | 部分（单测 fake；真实 Lua 缺键缺失） |

### 3.7 业务错误与查询语义

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| E1 | **P0** | 各类边界；限流/超时/依赖健康 | 未开始/已结束/商品不存在/库存不足/限流/队列满 | 对应 4xx/429；业务分支无 500 | HTTP | 存在未执行（单测；集成部分） |
| E2 | **P0** | 无该订单 | 按订单号查询 | `404 ORDER_NOT_FOUND` | HTTP | 存在未执行（`TestOrderNotFound`） |
| E3 | P1 | 查询限流触发 | 高频轮询 | `429` + `Retry-After` | HTTP | 部分（下单 429 已覆盖；查询限流缺失） |
| E4 | **P0** | 缺 `X-User-Id` | 下单/查询 | `401 UNAUTHENTICATED` | HTTP | 存在未执行（`TestSeckillRequiresUser`） |
| E5 | P1 | 幂等键非法 | 下单 | `400 INVALID_ARGUMENT` | HTTP | 存在未执行（单测） |
| E6 | **P0** | Redis 无记录、DB 无订单 | 按幂等键查询 | `409 RESULT_UNKNOWN`（非 404） | HTTP | 存在未执行（`TestRequestStatusSemantics`） |
| E7 | P1 | Redis 不可用且 DB 无订单 | 按幂等键查询 | `503`（非 404） | 注入 | 部分（单测） |

### 3.8 限流与队列容量

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| L1 | **P0** | `ingress` 限额小 | 首次后立即同键回放 | `429`；不扣库存、无查库副作用 | HTTP/Redis | 存在未执行（`TestA7GlobalRateLimitAppliesToReplay`） |
| L2 | P1 | 用户新单限额小 | 新意图 vs 回放 | 回放不消耗用户令牌；新意图超限 429 | HTTP + Redis | 部分（单测） |
| L3 | **P0** | `STREAM_MAX_ENTRIES` 小 | 填满后继续下单 | `429 QUEUE_FULL`；库存不变；XLEN 不超上限 | service/Lua | 存在未执行（`TestA7QueueFullDoesNotDeduct` + Lua 契约） |
| L4 | P1 | `query` 限额小 | 高频轮询 | 429 + `Retry-After` | HTTP | 缺失 |
| L5 | P1 | Stream 含已 ACK 记录 | 继续受理 | 仍计入上限；处理完成不自动恢复容量 | XLEN | 部分（Lua） |

### 3.9 可重复性与重启

| ID | P | 前置 | 操作 | 预期 | 验证方式 | 现有用例 |
|---|---|---|---|---|---|---|
| R1 | **P0** | 无外部依赖 | `go test -race ./...` | 单元通过，无数据竞争 | 命令输出 | 用例存在，本轮仅单元层执行 |
| R2 | **P0** | `*_test` 库/Redis | 压测脚本重复执行 | 自动校验订单≤库存、库存≥0、无重复订单 | `cmd/loadtest` + `verify.sql` | 脚本存在，未执行 |
| R3 | **P0** | 已预扣未落库 | 重启式初始化 | 不覆盖 Redis 库存 | Redis | 存在未执行（`TestA2RestartDoesNotOverwriteInFlightReservation`） |
| R4 | P1 | 压测 | 含轮询流量 | 报告入口与查询读压力；不声称已验证 RPS | 压测报告 | 部分（无断言） |

### 3.10 性能目标（仅目标，不作结论）

| ID | P | 目标 | 说明 | 验证方式 |
|---|---|---|---|---|
| P1 | P1 | 入口 1000 RPS、Worker 200 订单/秒 | **未验证目标**；记录机器配置、版本、并发、成功率、P99、错误分布 | `cmd/loadtest` + 监控/日志 |
| P2 | P1 | 压力下不超卖、不重复建单 | 必须同时校验 | `verify.sql` |

---

## 4. 现有测试对照与缺口

### 4.1 单元测试（无外部依赖）

- `internal/httpapi/api_test.go`：接受/默认 quantity、鉴权、未知字段、错误映射+`Retry-After`、REJECTED 回放、查询状态、列表越权、订单不存在、支付、管理员鉴权、ready、panic 兜底。
- `internal/service/service_test.go` + `business_test.go`：同键并发、全局限流优先、历史回放、Redis 故障回退、售罄、活动窗、输入边界、限购、状态语义、错误映射、取消回补、`requestId` 确定性；**Fix-7 专项**：`TestStaleRedisFingerprintDoesNotOverrideDBHistory`、`TestReserveConflictFallsBackToDBHistory`（Redis 指纹污染时以 MySQL 历史为准）。
- `internal/worker/worker_test.go`：DONE 只 ACK、拒绝补偿+DONE、成功回填、MISS 保留 pending、store 错误、未知 outcome；**补偿/坏消息均经 `processAndAck` 外层 ACK 逻辑验证**；补偿脚本异常不写 DONE、DONE 持久化失败不 ACK、`MarkSuccess` 缓存失败仍 ACK（DB 权威）、坏消息保留+告警日志；**结果不明**：`TestProcessUnknownCommitKeepsPendingThenUniqueResult`（首次不 ACK/不补偿，重试唯一结果并 ACK）。
- `internal/redisrepo/gateway_test.go`：Lua 契约（需 `SECKILL_REDIS_IT=1`）。

### 4.2 集成测试（需 `SECKILL_IT=1`；最终轮在 seckill_test + Redis db14 执行 25/25 通过）

- `acceptance_test.go`：A1 repo 层并发、A2 同键、A2b 限购、A3 取消回补、A4 认领+重投、支付/取消、拒绝补偿、端到端 service 流程、DB 最终防超卖。**守卫/隔离**：`SECKILL_IT_DSN` 必须为 `*_test`；`SECKILL_IT_REDIS_DB` 必须显式指定（最终轮 14）；取消用例先 `drainExpiredOrders`。
- `reliability_test.go`：A5 Redis 停/历史优先/MySQL 预扣前故障/结果不明；A2 删键回放/重启不覆盖；A3 标记缺失不双补；A4 提交未 ACK 重投/补偿崩溃窗口；额外预约；A7 全局限流含回放/队列满；支付取消竞争；**Fix-3 专项**：`TestPaymentRaceReturnsPayAfterCancel`；**Fix-8 专项**：`TestCompensationPartialExecutionDoesNotMarkDone`（真实 Redis，部分执行不误记 DONE）；**结果不明**：`TestUnknownCommitResultRecoversUnique`（真实 DB + 测试替身，重试唯一）。

### 4.3 缺口清单

| 缺口 | 影响 | 分类 | 建议 |
|---|---|---|---|
| 集成层不经过 HTTP 真栈 | 状态码/`Retry-After`/429 仅 fake 单测 | P1 | 增加 httptest + 真实依赖的 1 条 e2e |
| 真实停 Redis/MySQL | 当前用注入模拟 | P1/无法验证 | 人工演练；注入用例作为替代 |
| 真实 `SIGKILL` / 并发 `XAUTOCLAIM` | W5/W6 未覆盖 | P1/无法验证 | 人工脚本 + 双消费者用例 |
| MySQL 检查后故障 D3 | 无钩子 | P1/无法验证 | 测试钩子；不引入死信/自动恢复 |
| 真实 24h TTL | 用删键模拟 | 无法验证 | 删键/缩短配置替代 |
| 查询限流 L4 | 未覆盖 | P1 | 增加 HTTP 429 用例 |
| 支付/取消并发错误码 | 只断言终态 | 已补 | Fix-3 已修复；新增 `TestPaymentRaceReturnsPayAfterCancel` 断言 `PAY_AFTER_CANCEL` |
| Worker 退避/`attempts` | 未实现 | 需修复 | 见 Fix-2（P1，未处理） |
| 缓存权威性（Redis payload 先于 DB） | 可能误报冲突 | 已补 | Fix-7 已修复；新增 2 个单测 |
| 完整指标 | 无监控平台 | 已知限制 | 见第 7 节；不默认视为缺陷 |
| 1000 RPS / 200 单每秒 | 未验证 | 目标 | 压测报告标注目标/实测 |

---

## 5. 需要修复 / 已知限制 / 待确认

> 以下来自代码审阅与测试执行。只有登记在第 10 节的执行结果才可标记通过。

### 5.1 已修复并验证（本轮）

| 编号 | 问题 | 最小修复 | 复现/验证 | 结果 |
|---|---|---|---|---|
| Fix-3 | 支付/取消在 REPEATABLE READ 下用快照读再条件更新，竞争失败方返回 503 而非 `PAY_AFTER_CANCEL` | `internal/mysqlrepo/store.go` 的 `getOrderByNoTx` 改为 `... FOR UPDATE`（当前读） | 新增 `TestPaymentRaceReturnsPayAfterCancel`：原始事务持锁取消→提交→支付竞争，断言 `PAY_AFTER_CANCEL`；修复前实际得到 `inconsistent durable state` | 第 2 轮 PASS |
| Fix-7 | `Seckill` 在 Redis 幂等态 fingerprint 不一致时直接返回冲突，未先查 MySQL 历史 | `internal/service/service.go`：`state != nil` 分支先查 MySQL 历史；`Reserve` 返回 `CONFLICT` 时同样回查历史 | 新增 `TestStaleRedisFingerprintDoesNotOverrideDBHistory`、`TestReserveConflictFallsBackToDBHistory`；修复前均返回 `IDEMPOTENCY_CONFLICT` | 第 2 轮 PASS |
| Fix-8 | `compensate.lua`/`restock.lua` 先设标记再 `INCRBY`；库存值非整数时 `INCRBY` 失败但标记已存在，重试返回 `ALREADY`，Worker 可能误记 DONE | Lua 在设标记前校验库存值为整数，否则返回 `BAD_STOCK`（不设标记、不 `INCRBY`）；Worker 非 `OK`/`ALREADY` 保留 pending | Lua 契约 `TestCompensatePartialExecutionDoesNotFalselyComplete`（Redis db14）修复前因 marker 被设置而失败；修复后 PASS；集成 `TestCompensationPartialExecutionDoesNotMarkDone` PASS | 最终轮 PASS |

### 5.2 仍待处理（边界内与 SPEC 不一致，未在本轮修复）

| 编号 | 问题 | 依据 | 影响 | 优先级 |
|---|---|---|---|---|
| Fix-2 | Worker 失败仅记日志，无退避/`attempts`/`last_error`/阈值告警 | 4.3(5)(6) | 重试行为与可观测性不符；`reservation_results.attempts` 未写 | P1 |
| Fix-4 | Worker 事务先建 RR 快照、后 `FOR UPDATE`；订单读取用旧快照 | 4.3(2) | 并发同键不同预约可能先返回 `ErrInconsistent`，重试后才回填（不重复建单） | P2 |

### 5.3 已知限制（不要求新增机制）

| 编号 | 限制 | 依据 | 处理 |
|---|---|---|---|
| L-RedisRestock | 取消后 Redis 回补失败只告警，不做自动重试/恢复；MySQL 已正确回补，Redis 可能暂时少卖 | 4.6(3) | 列为已知限制；人工处理；24h marker 仅覆盖“若重试”的幂等，不要求实现重试 |
| L-Metrics | 无指标/监控平台；现有结构化日志提供告警证据（见第 7 节） | 4.6/7.2 | 默认不认定为缺陷；若验收要求“指标”，再单独立项 |

### 5.4 已澄清（不是缺陷）

- **预扣前 MySQL 检查**：下单在任何缓存读取前先执行 MySQL 幂等查询；MySQL 不可用会在此返回 503，不进入 Lua/缓存分支。原“缺少预扣前依赖检查”的判断撤回。检查通过后的故障按 G6b 结果不明处理，不要求无副作用。

边界外（S1–S7）只验证安全行为；不得为测试引入自动恢复、死信或自动对账。

---

## 6. 针对性补充测试（T1–T7）

| ID | P | 场景 | 前置 | 操作 | 预期 | 状态 |
|---|---|---|---|---|---|---|
| T1 | **P0** | 补偿脚本异常不误记完成 | 预约终态 `REJECTED`，`Compensate` 返回错误/异常 | Worker 处理 | 返回错误；不写 `compensation_state=DONE`；不 ACK；消息保留 | 单测经 `processAndAck` 外层：`TestProcessCompensationErrorKeepsPending` PASS |
| T2 | P1 | DONE 持久化失败不 ACK | 补偿成功但 `MarkCompensationDone` 报错 | Worker 处理 | 不 ACK；保持 pending；重复投递可重试 | `TestProcessMarkDoneErrorKeepsPending`（processAndAck）PASS |
| T3 | P1 | 历史订单不被非权威缓存覆盖 | DB 已有订单（payload A）；Redis `sk:req` 指纹被污染为 B | 同键同 payload A 重试 | 返回 DB 订单 `200`；不得返回 `IDEMPOTENCY_CONFLICT` | **Fix-7 已修复并验证**；`TestStaleRedisFingerprint...` + `TestReserveConflict...` PASS |
| T4 | **P0** | 坏消息保留+告警+不误确认 | Worker 处理失败（DB 错误/补偿 MISS） | 处理消息 | 不 ACK；输出保持 pending 告警日志；不写 DONE；不引入死信系统 | `TestProcessFailureLogsAndKeepsPending` + `TestProcessCompensationMISSKeepsPending`（processAndAck）PASS |
| T5 | P1 | 成功缓存失败不推翻订单 | DB 已提交订单，`MarkSuccess` 失败 | Worker 处理 | 仍 ACK；记录“DB authoritative”日志；订单有效 | `TestProcessMarkSuccessCacheFailureStillAcks` PASS |
| T6 | **P0** | 补偿 Lua 部分执行不误记完成 | 库存值非整数，`INCRBY` 不可能成功 | 调补偿并重试 | 返回 `BAD_STOCK`；不设 `sk:compensated`；重试不得返回 `ALREADY`；DB 保持 PENDING | Lua 契约（db14）+ 集成 `TestCompensationPartialExecutionDoesNotMarkDone` PASS |
| T7 | **P0** | 提交结果不明后恢复唯一 | 首次 `HandleReservation` 返回提交结果不明，DB 实际已提交 | Worker 首次处理 + 重试 | 首次不 ACK、不补偿；重试返回同一订单并 ACK；订单数仍 1；Redis 库存不增加 | 单测 `TestProcessUnknownCommitKeepsPendingThenUniqueResult` + 集成 `TestUnknownCommitResultRecoversUnique` PASS |

说明：T1/T4/T6 属于“坏消息/部分执行不误确认”的安全行为，不要求引入 DLQ；T3 暴露的 Fix-7 已修复；T6 暴露的 Fix-8 已修复；T7 验证结果不明时保留证据、恢复后结果唯一。Fix-3 另有专项集成用例 `TestPaymentRaceReturnsPayAfterCancel`。

---

## 7. 告警与日志证据（现有能力核查）

现有代码已具备以下结构化日志，可作为 MVP 约定的“告警证据”；**缺少监控平台不等于缺陷**，但需在交付说明中标注“仅日志、无指标”。

| 事件 | 位置 | 关键字段 | 证据用途 |
|---|---|---|---|
| Worker 处理失败保留 pending | `internal/worker/worker.go:101-107` | `messageId`、`reservationId`、`userId`、`itemId`、`claimed`、`error` | 坏消息/结果不明的告警 |
| 成功回填缓存失败（DB 权威） | `internal/worker/worker.go:130-132` | `reservationId`、`orderNo`、`error` | 缓存非权威证据 |
| 读组/认领/ACK 失败 | `internal/worker/worker.go:73,87,111` | `error`、`messageId` | 消费链路异常 |
| 取消回补失败 | `internal/service/service.go:405` | `orderNo`、`itemId`、`error` | Redis 回补失败告警 |
| 取消回补非 OK/ALREADY | `internal/service/service.go:409` | `orderNo`、`code` | 回补异常证据 |
| 取消调度失败 | `cmd/api/main.go`（`cancel expired orders failed`） | `error` | 调度异常 |

单元层已用 `TestProcessFailureLogsAndKeepsPending` 验证“失败→不 ACK→告警日志”。取消回补日志的自动化断言列为 P1。

---

## 8. 建议执行顺序（4～6 小时）

1. **静态检查（0 写库）**：`gofmt -l .`、`go vet ./...`、`go build ./...`。
2. **单元测试**：`go test -race ./internal/httpapi ./internal/service ./internal/worker`。
3. **全量单元**：`go test -race ./...`（无 env 时集成/Lua 自动跳过）。
4. **Lua 契约（独立 Redis）**：`SECKILL_REDIS_IT=1 SECKILL_IT_REDIS=... go test -race ./internal/redisrepo`。
5. **集成 P0（独立 `*_test` 库，需确认）**：串行 `-p 1`，按 A1→A2→A3→A4→A5→A6→A7。
6. **集成 P1**：HTTP 真栈、并发认领、查询限流、支付取消错误码回归（`TestPaymentRaceReturnsPayAfterCancel`）、T3 回归（`TestStaleRedisFingerprint...`/`TestReserveConflict...`）。
7. **人工故障演练（需授权）**：真实停 Redis/MySQL、`SIGKILL` Worker，核对 G6b/S2/S3 安全行为。
8. **压测（需授权）**：`cmd/loadtest` + `scripts/verify.sql`，记录目标/实测、环境、并发、P99、错误分布。

命令（执行者替换环境；禁止指向生产库）：

```bash
# 1. 静态 + 单元
gofmt -l . && go vet ./... && go build ./...
go test -race ./...

# 2. Lua 契约（独立 Redis 逻辑库）
SECKILL_REDIS_IT=1 SECKILL_IT_REDIS='127.0.0.1:6379' SECKILL_IT_REDIS_DB=14 \
  go test -race ./internal/redisrepo -v

# 3. 集成（必须 *_test 库；串行；需确认后执行）
SECKILL_IT=1 \
SECKILL_IT_DSN='root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4' \
SECKILL_IT_REDIS='127.0.0.1:6379' SECKILL_IT_REDIS_DB=14 \
  go test -race -p 1 ./internal/integration -v

# 4. 压测/演示（需确认写库/清库前提）
go run ./cmd/loadtest -base http://127.0.0.1:8080 -n 1000 -c 200 -item <id> -activity <id> -mode unique -poll
./scripts/demo_final.sh
```

---

## 9. 已知限制与不做

- 集成层验证 Service+Repo，不验证 HTTP 真栈（P1 可补一条 e2e）。
- 依赖故障用注入模拟，不等于真实宕机；真实停服走人工（P1）。
- Redis 数据丢失用定向删键模拟；不覆盖 AOF 丢页/flush/静默丢失全链路。
- 24h TTL 用删键/缩短配置模拟；不做真实等待。
- 不验证多实例/HA/分片/主从/Cluster；单实例假设。
- 不验证 exactly-once；只验证“重复消息不重复建单”。
- 不验证自动对账/修复/重建/DLQ/退款。
- 取消后 Redis 回补失败无自动重试/恢复（已知限制）。
- 无指标/监控平台；仅结构化日志（已知限制，见第 7 节）。
- 性能目标（1000 RPS、200 单/秒）仅待验证目标；报告必须标注“目标/实测/环境”，不得写成结论。

---

## 10. 执行记录

环境：macOS darwin/arm64；Go 1.27.4；MySQL `127.0.0.1:3306`；Redis `127.0.0.1:6379`。
数据隔离：MySQL 专用库 `seckill_test`（已授权复用，测试会 drain 历史过期 PENDING）；Redis 逻辑库：R1/R2 使用 db15（清理回 0），最终轮使用 db14（独占、测试用唯一前缀）。未连接 `seckill_lite` 或 db0。

最终演示环境与命令（实际执行）：

```bash
go build -o /tmp/seckill_demo_api ./cmd/api
HTTP_ADDR=:18080 \
MYSQL_DSN='root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4' \
REDIS_ADDR=127.0.0.1:6379 REDIS_DB=14 REDIS_PREFIX='sk:demo:final:' \
STREAM_KEY='sk:demo:final:stream:orders' STREAM_GROUP='demo-workers' \
ADMIN_TOKEN='demo-admin-token' ORDER_EXPIRE=60s CANCEL_INTERVAL=1s \
INGRESS_GLOBAL_RPS=100000 INGRESS_GLOBAL_BURST=100000 \
USER_NEW_ORDER_RPS=100000 USER_NEW_ORDER_BURST=100000 \
QUERY_GLOBAL_RPS=100000 QUERY_GLOBAL_BURST=100000 \
/tmp/seckill_demo_api

BASE_URL='http://127.0.0.1:18080' ADMIN_TOKEN='demo-admin-token' \
MYSQL_DB='seckill_test' REDIS_DB=14 REDIS_PREFIX='sk:demo:final:' \
./scripts/demo_final.sh
```

演示进程已在结束后停止；`db14` 残留 62 个 `sk:demo:final:*` 键，未删除（待授权清理）。

| 轮次/日期 | 命令（摘要） | 范围 | 结果 | 备注 |
|---|---|---|---|---|
| R1 2026-11-19 | `gofmt -l .` / `go vet ./...` / `go build ./...` | 静态 | PASS | 无输出 / exit 0 |
| R1 2026-11-19 | `go test -race ./...` | 单元 | PASS | 未设 env 时集成/Lua 用例内部 skip（包编译通过） |
| R1 2026-11-19 | `SECKILL_REDIS_IT=1 ... SECKILL_IT_REDIS_DB=15 go test -race ./internal/redisrepo -v` | Lua 契约 | PASS | `TestLuaContracts` |
| R1 2026-11-19 | `SECKILL_IT=1 SECKILL_IT_DSN=.../seckill_test... SECKILL_IT_REDIS_DB=15 go test -race -p 1 ./internal/integration -v` | 集成 P0 | PASS 22/22 | 5.8s；db15 清理回 0 |
| R1 2026-11-19 | 上述集成（观察） | Fix-3/Fix-7 | 未覆盖 | 现有用例未断言错误码/污染指纹；仅代码审阅判断存在 |
| 修复前复现 | `go test -race ./internal/service -run 'TestStaleRedisFingerprint\|TestReserveConflictFallsBack'` | Fix-7 复现 | **FAIL** | 实际返回 `IDEMPOTENCY_CONFLICT` |
| 修复前复现 | `... go test -race -p 1 ./internal/integration -run TestPaymentRaceReturnsPayAfterCancel` | Fix-3 复现 | **FAIL** | 实际返回 `inconsistent durable state`（映射 503） |
| R2 2026-11-19 | `gofmt -l .` / `go vet ./...` / `go build ./...` | 静态 | PASS | 修复后 |
| R2 2026-11-19 | `go test -race -count=1 ./...` | 单元 | PASS | 禁用缓存 |
| R2 2026-11-19 | `... ./internal/service -run 'TestStaleRedisFingerprint\|TestReserveConflictFallsBack'` | Fix-7 验证 | PASS 2/2 | 返回 DB 历史 |
| R2 2026-11-19 | `... ./internal/worker -v` | 补偿/坏消息回归 | PASS 10/10 | 无误写 DONE/误 ACK |
| R2 2026-11-19 | `... ./internal/redisrepo -v` | Lua 契约 | PASS | db15 |
| R2 2026-11-19 | `SECKILL_IT=1 ... -p 1 ./internal/integration -v` | 集成 P0 + Fix-3 | PASS 23/23 | 6.15s；含 `TestPaymentRaceReturnsPayAfterCancel` |
| 最终轮 2026-11-19 | `gofmt -l .` / `go vet ./...` / `go build ./...` | 静态 | PASS | 最终代码 |
| 最终轮 2026-11-19 | `go test -race -count=1 ./...` | 单元 | PASS | 含结果不明/ACK 外层新单测 |
| 最终轮 2026-11-19 | `SECKILL_REDIS_IT=1 ... SECKILL_IT_REDIS_DB=14 go test -race ./internal/redisrepo -v` | Lua 契约 | PASS | 含 `TestCompensatePartialExecutionDoesNotFalselyComplete` |
| 修复前复现 | `... ./internal/redisrepo -run TestCompensatePartialExecutionDoesNotFalselyComplete` | Fix-8 复现 | **FAIL** | 库存非整数时 marker 已设置 |
| 最终轮 2026-11-19 | `SECKILL_IT=1 ... /seckill_test... SECKILL_IT_REDIS_DB=14 go test -race -p 1 ./internal/integration -v` | 集成 | PASS 25/25 | 6.16s；含 Fix-3/Fix-8/结果不明 |
| 最终轮 2026-11-19 | `scripts/demo_final.sh`（API :18080，seckill_test + db14） | 最小演示 | PASS | 200 请求/50 库存：accepted=50、sold_out=150、db_orders=50、stock=0；支付 `PAID`；冲突 `409`；超时取消 `CANCELLED`、DB/Redis 各回补 1、CANCEL 流水 1 |
| 未执行 | `golangci-lint run ./...` | 静态 | 未执行 | 未安装/未运行，不声称通过 |
| 未执行 | 真实停 Redis/MySQL、`SIGKILL`、HTTP 真栈、压测/RPS、查询限流 L4、并发 `XAUTOCLAIM`、D3 钩子 | P1/无法验证 | 未执行 | 不计入通过 |

已知数据残留：Redis `db0` 有 5 个旧版测试硬编码 DB0 遗留的 `sk:test:*` 键（非本轮写入，未清理，待授权处理）。`seckill_test` 在最终轮后约 1283 单（1227 PENDING，其中 50 过期；24 PAID / 32 CANCELLED）；下次集成运行由 `drainExpiredOrders` 清理过期项，清库需另行确认。Redis `db14` 有 62 个 `sk:demo:final:*` 演示键（本轮写入，未删除，待授权清理）。

记录要求：
- 写明机器配置、依赖版本、并发参数、数据规模。
- 压测同时给出目标值与实测值，不得把目标写成结论。
- 失败项附日志/错误码/复现步骤；未执行项（真实停服、杀进程、24h TTL、RPS）必须显式列出。
- **测试存在不等于测试通过**：只有在本表登记且记录命令/环境/结果，才可标记通过。
