# AGENTS.md

本文件是项目级补充约定，与全局 `~/.config/opencode/AGENTS.md` 同时生效；冲突时以本文件关于本项目的约束为准。

## 1. 事实来源

- 需求、接口语义、状态机、失败边界以 `docs/SPEC.md` 为准。
- 任何改变以下内容的实现，必须先更新 SPEC：
  - 库存扣减位置或防超卖方式；
  - `202` / 轮询 / 幂等语义；
  - Redis/MySQL 故障时的行为；
  - 取消回补、支付、结果不明的处理；
  - 验收用例与目标值。
- 不得在 README、代码注释或汇报中声称“已实现自动对账、自动重建、完整高可用、已验证 1000 RPS/200 单每秒”等 SPEC 未包含的能力。

## 2. 当前阶段

- 当前只有需求文档与项目约定；业务代码尚未实现。
- 未经用户明确确认，不写业务代码、不执行数据库迁移/SQL、不清库、不启动压测写库。
- 不删除项目文件，不删除或改写原题；新增文档放在 `docs/`。

## 3. 架构边界（实现时）

```text
transport/http -> service -> repository(mysql/redis)
                       \-> worker/job（Stream 消费、取消、支付回执）
```

- `cmd/`：仅入口与依赖装配、生命周期。
- `internal/`：业务逻辑；不得从模块外导入。
- `internal/transport/http`：只做校验、鉴权、参数映射、错误映射；不直接写订单/库存。
- `internal/service`：用例编排；不直接操作 `http.Request`/`ResponseWriter`。
- `internal/repo/mysql`：事务、条件更新、唯一约束；SQL 只在此层。
- `internal/repo/redis`：Lua 脚本、Stream、限流、幂等状态；Lua 放 `internal/lua/` 并 `go:embed`。
- 每层依赖小接口，接口定义在使用方。

## 4. 不可破坏的业务不变量

1. **MySQL 是订单/库存/支付/取消的唯一事实来源**；Redis 只是准入闸门和异步缓冲。
2. **防超卖最终保障**：MySQL 同一事务内执行 `available_stock >= quantity` 条件扣减；库存不得为负；订单数不得超过库存。
3. **Redis 或 MySQL 故障时，新订单快速失败（503）**；禁止绕过 Redis 直接写库下单，禁止在 Redis 不可用时“降级为纯 DB 下单”。
4. **入口必须要求 `Idempotency-Key`**，按用户隔离；同一键 + 同一 payload 返回同一结果，不同 payload 返回 `IDEMPOTENCY_CONFLICT`。
5. **`202` 仅表示已受理**，不等于成功；客户端必须通过幂等键轮询。查询语义必须区分 `PROCESSING`、订单结果、`REJECTED`、`RESULT_UNKNOWN`、依赖不可用。
6. **Redis Stream 是至少一次投递**：`XACK` 只能在 MySQL 事务提交成功后执行；重复消息依赖 `UNIQUE(user_id, idempotency_key)` 与回查去重。
7. **结果不明时禁止自动回补/判失败**：不得因为超时、查询不到订单、Worker 未处理就回补库存；保留消息、暂停处理、告警、按 SPEC 7.3 人工核查。
8. **取消只针对已落库且过期的 `PENDING_PAYMENT` 订单**；DB 回补必须一次（条件状态更新 + `UNIQUE(order_no, type)`）；Redis 回补受一次性标记保护，失败仅告警，不自动修复。
9. **支付与取消通过条件更新竞争**；重复支付幂等；已取消订单支付返回 `PAY_AFTER_CANCEL`，不做真实退款。
10. **不引入 SPEC 之外的基础设施**（Kafka、分片、跨机房、自动对账服务等）；不新增依赖，除非用户明确要求。

## 5. 错误与日志

- 业务分支使用明确错误码，不得返回 500；未预期 panic 由中间件兜底并告警。
- 错误包装用 `fmt.Errorf("context: %w", err)`；禁止用 panic 做控制流。
- 结构化日志使用 `log/slog`；禁止 `fmt.Print*` 记录日志。
- 关键日志字段：`requestId`、`idempotencyKey`、`orderNo`、`itemId`、`userId`、`messageId`、`deliveryCount`、`errorCode`。
- 不记录支付密钥、完整请求体中的敏感信息。

## 6. 并发与资源

- 所有 I/O/RPC/DB/Redis 调用第一个参数是 `ctx context.Context`。
- 每个 goroutine 必须有退出路径；服务关闭时先停止接收新单，再停止 Worker/取消任务。
- HTTP、MySQL、Redis、Worker 均需超时；禁止无界重试和无界队列。
- Worker 重试必须退避；达到阈值保留 pending 并告警，不得静默丢弃。
- 不使用内存状态作为订单/库存正确性的依赖；重启后必须能从 MySQL 恢复。

## 7. 测试要求

实现阶段必须覆盖：

- 单元测试：并发扣减（不超卖）、幂等建单、取消回补一次、错误映射、Redis 不可用降级/拒绝。
- 集成测试（`SECKILL_IT=1`，本地 MySQL/Redis）：A1 并发不超卖、A2 幂等、A3 超时取消回补、A4 Worker 重启重试、A5 依赖不可用与结果不明。
- 压测脚本：`N` 并发抢 `M` 库存（N >> M），自动校验订单数 ≤ 库存、库存非负、无重复订单。
- 目标值 1000 入口 RPS / 200 订单每秒只能作为目标标注，报告必须写明机器配置与实测结果，不能把目标写成结论。
- 幂等回源与状态轮询的 DB 读压力必须出现在压测/说明中。

验证命令（代码存在后必须执行）：

```bash
gofmt -l .
go vet ./...
golangci-lint run ./...   # 若已安装
go test -race ./...
```

## 8. 数据与安全

- 不编辑已有迁移文件；新增迁移追加编号。执行迁移/压测写库前需用户确认。
- 不使用真实支付、真实退款、真实个人信息。
- 不提交密钥、DSN 密码、`.env`；配置从环境变量读取，示例用占位符。
- 运营接口在 MVP 中用演示令牌/内网假设保护，不得默认暴露到公网。

## 9. 文档同步

- `README.md`：只维护项目简介、运行方式、SPEC 链接、明确的“已实现/未实现”状态。
- `docs/SPEC.md`：语义变化的唯一更新点。
- API 示例、状态码、DDL 与 SPEC 不一致时，先修 SPEC 再改实现与 README；不得只改代码不改文档。
- 压测报告标注“目标值/实测值”，并注明日期、环境与脚本参数。
