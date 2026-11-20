# 初稿规范符合性与测试充分性审查

**Session ID:** ses_e10b7ec69ffeYo51FISuR8S2PU
**Created:** 11/18/2026, 11:30:40 PM
**Updated:** 11/19/2026, 12:26:12 AM

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

```
Tool: edit

```

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
Tool: write

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

已按 7 点修订，更新了 `docs/TEST_PLAN.md`，并同步了测试隔离与针对性单测。未改业务代码，未运行写库/写 Redis 的测试。

## 对应修订

1. **Fix-1 撤回**：确认 `Seckill` 在任何缓存读取前先执行 MySQL 幂等查询（`lookupHistoryGuarded → store.LookupRequest`）。MySQL 预扣前不可用会在该步返回 503，不进入 Lua；检查通过后故障归入 G6b 结果不明，不要求无副作用。
2. **取消回补失败**：从“需修复”改为已知限制，明确不新增自动重试/恢复；24h marker 仅覆盖“若重试”的幂等。
3. **测试隔离**：在 `setup` 增加 `SECKILL_IT_DSN` 必填 + 库名 `*_test` 守卫（连接前失败）；新增 `drainExpiredOrders`，取消类用例在创建被测订单前清空存量过期订单；文档要求专用测试库、`-p 1` 串行、无 `t.Parallel`，不再只靠“断言本单”。
4. **预扣前后区分**：G6a/G6b 拆分；D2（预扣前）要求 503 + 无预扣/无 Stream；D3（入队后）只要求保留预约、恢复后同键可得结果，不要求无副作用。
5. **收敛 P0**：P0 = 不超卖、同键幂等、取消回补、消息重投/补偿、依赖故障安全；真实停服/杀进程/完整指标/大规模压测/HTTP 真栈/查询限流降为 P1。C1“恰好售罄”写明限流额度、超时、依赖健康、Worker 排空等前置。
6. **针对性测试**：
   - 新增单测：补偿脚本异常不写 DONE、DONE 持久化失败不 ACK、`MarkSuccess` 缓存失败仍 ACK、坏消息保留+告警日志（无 DLQ）。
   - 文档新增 T1–T5；其中 **T3“历史订单不被非权威缓存覆盖”当前代码不满足**，已列为 **Fix-7**（Redis payload 指纹先于 DB 判定），修复前不纳入门禁。
7. **告警证据**：核查并记录现有结构化日志位置与字段（Worker 保留 pending、`MarkSuccess` 失败、读组/认领/ACK 失败、取消回补失败/非 OK）；明确“无监控平台”列为已知限制而非默认缺陷，并强调“测试存在 ≠ 测试通过”。

## 校验

- `gofmt -l .` 无输出、`go vet ./...` 通过、`go test -race ./...` 通过（集成/Lua 因未设 env 跳过，未写库）。
- 改动文件：`docs/TEST_PLAN.md`（新增）、`internal/integration/acceptance_test.go`、`internal/integration/reliability_test.go`、`internal/worker/worker_test.go`。

T3 需要先修 `internal/service/service.go` 的缓存权威性（Fix-7）。是否要我在你确认后修这一项并补上对应测试？

---

## User

请作为主代理协调开发和测试，目标是在当前 MVP 的 SPEC 边界内完成验证，不扩展需求。

先读取 docs/SPEC.md、docs/TEST_PLAN.md 和 AGENTS.md，确认现有代码与工作区状态。不要覆盖或丢弃已有未提交修改。

请启动一个测试子代理，按以下循环工作：

1. 选定当前待测版本或工作区快照；测试期间暂停修改同一批文件。
2. 测试子代理负责执行 P0 测试并报告：测试对象、命令、结果、复现步骤、预期与实际、问题是否在 SPEC 边界内。它不得修改业务代码。
3. 你核对报告：边界内的缺陷先补可复现测试，再做最小修复；边界外问题只验证安全失败、记录限制，不引入自动恢复、死信系统或新基础设施。
4. 修复完成后，通知同一个测试子代理重新验证修复和相关回归；必要时继续下一轮。
5. 最多循环 3 轮。全部 P0 通过、遇到阻塞、需要调整业务边界或达到轮次上限时停止，汇总通过/失败/跳过的测试与未解决问题，不要声称未执行的测试通过。

所有测试必须使用独立测试库和 Redis，禁止指向生产数据；涉及迁移、写库、清库、停服务等操作，先遵守项目已有确认要求。不要自动提交或推送。
执行集成测试前，确认使用独立测试库和独立 Redis，且不含需要保留的数据。测试过程中发现 Fix-3、Fix-7 或补偿误记完成的问题，先复现并判断是否违反 SPEC，再做最小修复；不能仅因为列为 P1 或不在门禁中就忽略。记录每项实际执行的命令、结果和未执行原因。 

---

## Assistant

```
Tool: read

```

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

```
Tool: bash

```

---

## Assistant

```
Tool: bash

```

```
Tool: bash

```

---

## Assistant

环境核查结果（只读，未写入）：

- MySQL `seckill_test` 存在且表齐全；其中 `orders=875`、`PENDING_PAYMENT=844`，**全部已过期**（另有 15 PAID、16 CANCELLED）。这些看起来是历史测试残留。
- Redis 仅有 `127.0.0.1:6379` 单实例，`db0` 有 949 个键；`db13/14/15` 均为空。
- 工作区未提交修改仍在（3 个测试文件 + `docs/TEST_PLAN.md`），未覆盖。

按“执行集成测试前确认独立测试库/独立 Redis、且不含需保留数据”的要求，需要你确认两点后再启动子代理跑写库用例：

```
Tool: question

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: read

```

---

## Assistant

```
Tool: edit

```

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
Tool: task

```

---

## Assistant

第 1 轮子代理报告：P0 已执行部分全绿；但确认 Fix-7、Fix-3 仍在代码路径中。按你的要求先补可复现测试，再做最小修复。

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
Tool: edit

```

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
Tool: edit

```

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
Tool: task

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: read

```

```
Tool: read

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: read

```

---

## Assistant

```
Tool: edit

```

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

# MVP 验证汇总（2 轮，已停止）

全部 P0（已执行范围）通过；Fix-3、Fix-7 已按“先复现、后最小修复”处理并回归通过；补偿误记完成/误 ACK 未复现。达到通过条件，未进入第 3 轮。未提交、未推送。

## 1. 工作区与边界

- HEAD 测试期间被外部更新为 `7226fee 测试计划`（仅 `docs/TEST_PLAN.md`），非本次操作；未提交/推送。
- 未覆盖或丢弃既有修改：`docs/TEST_PLAN.md`、3 个集成/worker 测试文件、`service.go`、`store.go`、`gateway_test.go`、`business_test.go` 均保留在工作区。
- 数据：MySQL `seckill_test`（专用测试库）；Redis `db15`（测试前后均为 0）；未连接 `seckill_lite`，未写 db0。

## 2. 循环记录

| 轮次 | 测试对象 | 命令（摘要） | 结果 |
|---|---|---|---|
| R1 | 静态+单元+Lua+集成 22 项 | `gofmt`/`vet`/`build`、`go test -race ./...`、`SECKILL_REDIS_IT=1 ... -n15`、`SECKILL_IT=1 ... -p 1 ./internal/integration` | 全 PASS；22/22 |
| 复现 | Fix-7（单元） | `go test -race ./internal/service -run 'TestStaleRedisFingerprint\|TestReserveConflictFallsBack'` | **FAIL**：实际 `IDEMPOTENCY_CONFLICT`，预期返回 DB 历史 |
| 复现 | Fix-3（集成） | `... -run TestPaymentRaceReturnsPayAfterCancel` | **FAIL**：实际 `inconsistent durable state`（映射 503），预期 `PAY_AFTER_CANCEL` |
| R2 | 修复后回归 | 同上全量，`go test -race -count=1 ...`、集成 `-p 1` | 全 PASS；集成 23/23 |

## 3. 最小修复（对齐现有 SPEC，未改语义）

- **Fix-7** `internal/service/service.go`：Redis 幂等态命中时先查 MySQL 历史，命中即返回；`Reserve` 返回 `CONFLICT` 时同样回查历史。依据 SPEC 4.2(3)/6.3。新增单测 `TestStaleRedisFingerprintDoesNotOverrideDBHistory`、`TestReserveConflictFallsBackToDBHistory`。
- **Fix-3** `internal/mysqlrepo/store.go`：`getOrderByNoTx` 改为 `... FOR UPDATE`，支付与取消竞争时读取最新已提交状态。依据 SPEC 4.5/5.4。新增确定性集成用例 `TestPaymentRaceReturnsPayAfterCancel`（原始事务持锁取消→提交→支付，断言 `PAY_AFTER_CANCEL`）。
- **补偿误记/误 ACK**：回归 `TestProcessCompensationErrorKeepsPending`、`TestProcessMarkDoneErrorKeepsPending`、`TestProcessCompensationMISSKeepsPending`、`TestProcessFailureLogsAndKeepsPending`、`TestProcessMarkSuccessCacheFailureStillAcks` 及集成补偿崩溃窗口，全部 PASS；未发现违规。

## 4. P0 通过清单（已执行）

- 静态：`gofmt -l .` 无输出、`go vet ./...`、`go build ./...` 通过。
- 单元：`go test -race -count=1 ./...` 通过；worker 10/10。
- Lua 契约：`TestLuaContracts` 通过（db15）。
- 集成 23/23：A1 不超卖、A2 幂等/限购/删键回放/重启不覆盖、A3 取消回补/标记缺失不双补、A4 认领/重投/补偿崩溃窗口、A5 Redis 停/历史优先/MySQL 预扣前故障/结果不明、A7 限流含回放/队列满、支付幂等/取消、DB 最终防超卖、额外预约、Fix-3 专项。

## 5. 失败 / 跳过

- 失败：无（修复后）。
- 跳过：未设 env 时集成/Lua 用例内部 skip（符合预期，已在 R1/R2 用真实测试库重跑）。
- **未执行，不声称通过**：`golangci-lint`、真实停服/`SIGKILL`、HTTP 真栈 e2e、压测/RPS、查询限流 L4、并发 `XAUTOCLAIM`、D3“检查后 MySQL 故障”钩子。

## 6. 未解决 / 已知限制

- **Fix-2（P1，未修）**：Worker 无退避/`attempts`/`last_error`/阈值告警（SPEC 4.3(5)(6)）。
- **Fix-4（P2，未修）**：Worker 事务旧快照读，可能偶发 `ErrInconsistent`（不重复建单，靠重试自愈）。
- Redis 取消回补失败无自动重试；无指标平台、仅结构化日志（已知限制，按约定不扩展）。
- `seckill_test` 累积约 1110 单（1064 过期 PENDING）；下次运行由 `drainExpiredOrders` 清理，彻底清库需另行确认。
- Redis `db0` 有 5 个旧版测试硬编码 DB0 遗留的 `sk:test:*` 键（非本轮写入，未清理，待授权）。

## 7. 文档

`docs/TEST_PLAN.md` 已同步：Fix-3/Fix-7 标记“已修复并验证”、T3 解除阻塞、补充新增测试与第 10 节两轮实际命令/结果/未执行项。

需要你确认后我才能执行：db0 旧键清理、`seckill_test` 清库、以及是否继续处理 Fix-2/Fix-4。未自动提交或推送。

---

## User

请以当前最新代码、面试题、docs/SPEC.md 和 docs/TEST_PLAN.md 为准，完成秒杀 MVP 的最后验收与交付。不要重新设计架构，不引入 Kafka、DLQ、自动对账、自动重建、库存分片或真实支付。

1. 先核对当前工作区与测试记录，列出最终要求中已完成、待验证和未实现的内容；不要把旧结论或测试存在当作当前已通过。
2. 优先补齐并验证边界内的关键场景：
   - 并发不超卖、同键幂等、超时取消只回补一次；
   - 已受理消息处理时发生提交结果不明：不盲目回补、不误 ACK；恢复重试后结果唯一。可以使用测试替身或故障钩子，不要求真实停服；
   - 补偿 Lua 发生部分执行时，不能把未完成回补误记为完成；使用独立 Redis 验证。
   - 检查补偿失败测试是否经过实际 ACK 外层逻辑，而不是只调用内部处理函数。
3. 若测试暴露边界内缺陷，先复现并补测试，再做最小修复。边界外异常只要求安全失败、保留证据与人工处理，不增加自动恢复系统。
4. 测试使用本轮独占的测试库和 Redis；确认不包含需保留的数据。迁移、清理或停服务前遵循现有确认要求，不触碰运行库。
5. 按原题完成最小演示：启动、创建活动、并发抢购、查询结果、支付或超时取消，以及至少一条异常路径。记录实际命令、环境、结果和未执行原因。
6. 更新 README 和测试记录，明确已实现/已验证/未验证、已知限制，以及 Prompt 记录。1000 入口请求/秒和 200 新订单/秒仅在实测达到后才能写为结果。

完成后按“已完成、验证结果、未解决问题、演示步骤”简短汇报。不要自动提交或推送。 

---

## Assistant

```
Tool: read

```

```
Tool: bash

```

---

## Assistant

```
Tool: read

```

```
Tool: read

```

```
Tool: read

```

---

## Assistant

```
Tool: question

```

---

## Assistant

当前工作区已由外部提交为 `2bf6dbd 第一轮测试`（工作区干净，未丢改动）。开始针对本轮要求补齐“结果不明 + 补偿部分执行 + ACK 外层”测试，先复现再修复。

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: read

```

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
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: read

```

---

## Assistant

```
Tool: edit

```

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
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

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
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: read

```

---

## Assistant

```
Tool: bash

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

---

## Assistant

```
Tool: edit

```

```
Tool: edit

```

---

## Assistant

```
Tool: write

```

---

## Assistant

```
Tool: read

```

```
Tool: bash

```

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
Tool: bash

```

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
Tool: edit

```

---

## Assistant

# 最终验收与交付

## 已完成

- 核对工作区/提交：HEAD `2bf6dbd 第一轮测试`；本轮修改未提交、未推送，未覆盖既有改动。
- 边界内关键场景补齐并验证：
  - 并发不超卖、同键幂等、超时取消只回补一次（回归通过）。
  - **提交结果不明**：新增单测 `TestProcessUnknownCommitKeepsPendingThenUniqueResult` + 集成 `TestUnknownCommitResultRecoversUnique`；首次不 ACK/不补偿，重试返回唯一订单、订单数不增、Redis 库存不增。
  - **补偿 Lua 部分执行**：独立 Redis 复现（marker 先于 `INCRBY` 被设置，重试误报 `ALREADY`），最小修复 `compensate.lua`/`restock.lua`：库存非整数时返回 `BAD_STOCK`、不设标记；新增 Lua 契约 + 集成测试。
  - **ACK 外层逻辑**：补偿失败/MISS/DONE 持久化失败测试改为经 `processAndAck`，断言不 ACK、不误记 DONE。
- 文档更新：`README.md`、`docs/TEST_PLAN.md`、`docs/REDIS_KEYS.md`；新增 `docs/PROMPT_LOG.md`、`scripts/demo_final.sh`。

## 验证结果（本轮实际执行）

| 命令 | 结果 |
|---|---|
| `gofmt -l .` / `go vet ./...` / `go build ./...` | 通过 |
| `go test -race -count=1 ./...` | 通过 |
| `SECKILL_REDIS_IT=1 ... SECKILL_IT_REDIS_DB=14 go test -race ./internal/redisrepo -v` | 通过（含 `TestCompensatePartialExecutionDoesNotFalselyComplete`） |
| `SECKILL_IT=1 .../seckill_test... SECKILL_IT_REDIS_DB=14 go test -race -p 1 ./internal/integration -v` | **25/25 通过** |
| `scripts/demo_final.sh`（:18080，seckill_test + db14） | 通过 |

演示实测：200 请求 / 50 库存 → `accepted=50`、`sold_out=150`、`db_orders=50`、`available_stock=0`；支付 `PAID`；同键不同 payload `409 IDEMPOTENCY_CONFLICT`；超时取消 `CANCELLED`，DB 库存回补 1、`CANCEL` 流水 1、Redis 库存 1、restocked marker 1。

未验证（不声称通过）：1000 RPS / 200 单每秒、真实停服/`SIGKILL`、HTTP 真栈、多实例并发认领、真实 24h TTL、`golangci-lint`、真实 MySQL 入队后故障停服。

## 未解决问题

- **Fix-2（P1）**：Worker 无退避/`attempts`/`last_error`/阈值告警，未修复。
- **Fix-4（P2）**：Worker 事务旧快照读可能偶发 `ErrInconsistent`，重试可恢复，未修复。
- 取消后 Redis 回补失败仅告警、不自动重试；无指标平台（已知限制，未扩展）。
- 数据残留：`db14` 有 62 个 `sk:demo:final:*` 演示键；`seckill_test` 约 1283 单（1227 PENDING，50 过期）；`db0` 有 5 个旧版遗留键。均未清理，待你确认。
- 本轮未触碰运行库 `seckill_lite`/db0。

## 演示步骤

```bash
go build -o /tmp/seckill_demo_api ./cmd/api
HTTP_ADDR=:18080 \
MYSQL_DSN='root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4' \
REDIS_DB=14 REDIS_PREFIX='sk:demo:final:' STREAM_KEY='sk:demo:final:stream:orders' \
STREAM_GROUP='demo-workers' ADMIN_TOKEN='demo-admin-token' ORDER_EXPIRE=60s CANCEL_INTERVAL=1s \
INGRESS_GLOBAL_RPS=100000 INGRESS_GLOBAL_BURST=100000 \
USER_NEW_ORDER_RPS=100000 USER_NEW_ORDER_BURST=100000 \
QUERY_GLOBAL_RPS=100000 QUERY_GLOBAL_BURST=100000 \
/tmp/seckill_demo_api

BASE_URL=http://127.0.0.1:18080 ADMIN_TOKEN=demo-admin-token \
MYSQL_DB=seckill_test MYSQL_DSN='root@tcp(127.0.0.1:3306)/seckill_test?parseTime=true&loc=UTC&charset=utf8mb4' \
REDIS_DB=14 REDIS_PREFIX='sk:demo:final:' ./scripts/demo_final.sh
```

演示进程已停止；未自动提交或推送。

---

