# seckill_lite

Go + MySQL + Redis 轻量级秒杀系统。

- 需求、接口、状态与验收标准：[docs/SPEC.md](docs/SPEC.md)
- 接口契约：[docs/API.md](docs/API.md)
- 数据库设计：[docs/SCHEMA.md](docs/SCHEMA.md) / [migrations/0001_init.sql](migrations/0001_init.sql)
- Redis Key 与 Lua 契约：[docs/REDIS_KEYS.md](docs/REDIS_KEYS.md)
- 当前状态：MVP 规格与接口/表结构设计已收敛，业务代码尚未实现，DDL 未执行。
- 性能目标：入口 1000 RPS / 新订单 200 每秒（目标值，尚未验证）。
