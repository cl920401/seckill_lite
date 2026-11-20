# 2026-11-19 UTC+8 独立实测
版本af71255。Apple M4/16GiB、macOS arm64、Go1.27.4、MySQL8.4.13、Redis7.4.12，同机应用/依赖，仅新建专用*_test库及Redis DB13唯一前缀。未改业务代码。

## 自动化命令与结果
- gofmt -l .：空输出；go vet ./...、go build ./...：exit0。
- env -u SECKILL_IT -u SECKILL_REDIS_IT go test -race -count=1 -v ./...：HTTP12+Service22+Worker11=45顶级单测通过（另3子用例）；此命令下25集成+2Lua跳过。
- SECKILL_REDIS_IT=1 SECKILL_IT_REDIS=127.0.0.1:6379 SECKILL_IT_REDIS_DB=13 go test -race -count=1 -v ./internal/redisrepo：2/2通过，1.201秒。
- SECKILL_IT=1 SECKILL_IT_DSN=<独占测试库DSN> SECKILL_IT_REDIS=127.0.0.1:6379 SECKILL_IT_REDIS_DB=13 go test -race -count=1 -p 1 -v ./internal/integration：25/25通过，6.283秒。
- golangci-lint未安装，未执行。

## 真实HTTP
health/ready；创建→202→查询→支付PAID；同键不同payload409。200请求抢50库存：50受理/150售罄，DB50单/库存0。超时取消CANCELLED，DB与Redis各回补1、CANCEL流水1、标记1。

## 两个独立10秒性能采样
默认限流；不是混合或长期负载。200新单/s：2000请求全部202，199.96RPS，P50 2.28ms/P99 3.81ms；2000订单落库首末跨度9.99s，库存3000→1000。
1000幂等回放/s：10000请求全部200，999.85RPS，P50 1.64ms/P99 81.18ms；订单仍2000。
延迟为HTTP入口响应，不是落库端到端延迟。

## 1万独立请求
并发200、库存1000：1000受理202/1123售罄409/7877限流429，网络错误及5xx=0。请求阶段0.970s，P50 15.68ms/P99 52.94ms/max85.23ms。响应10309/s包含90%快速拒绝，不是万单吞吐。最终1000订单/库存0；落库首末跨度2.334s，仅短突发约428单/s，非持续能力。

## 两异常Case
同用户同键50并发：49个202、1个429；最终1订单、quantity合计1、DEDUCT流水1，DB/Redis库存10→9；等待1.1s同键重试返回200同一订单。
运行时Redis不可用：API经本轮TCP代理连Redis，健康后仅停代理，不停Redis。新单503 SERVICE_UNAVAILABLE，2.101s；DB订单3171→3171、request_results3177→3177、商品DB/Redis库存10→10。无预扣/无绕过落库；不能称毫秒级失败。

## 证据边界
相关查询负库存0、重复业务键0；处理后Pending0/lag0。25集成覆盖不超卖、幂等/限购、取消、认领/重投/补偿、支付竞争、依赖失败/结果不明、回源、重启保库存、限流/队列、部分补偿。错误映射主要由单元层覆盖。
未测同时混合负载、长期容量、真实停服务/SIGKILL、多实例认领、真实24hTTL、完整端到端延迟分布。Fix-2/Fix-4仍见README，不因测试通过而消失。
测试库保留；自启服务/代理均结束，仅清理本轮Redis前缀，DB13回0键。临时性能工具为Python标准库HTTP客户端+线程+单调时钟；项目cmd/loadtest本身没有P99统计。本文是实际执行摘要，不冒充原始输出全集。
