-- 订单取消回补：调用前必须已在 DB 完成取消及库存回补；本脚本只同步 Redis。
-- KEYS：1=商品库存；2=restocked:{orderNo}。
-- ARGV：1=回补数量；2=标记 TTL（秒，默认由 Go 层传入 86400）。
-- 此标记按订单去重，区别于预约补偿的永久 marker；过期不构成再次回补的授权。
-- 正常取消调度依靠 DB 的 CANCELLED 终态避免重做，人工补偿须先核对流水和库存。
local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'MISS'}
end
-- 当前只排除不能转成数字的值，未严格排除小数、非整数格式和加法溢出。
if not tonumber(redis.call('GET', KEYS[1])) then
  -- 在这类明确异常下不设置标记，保持“尚未确认回补”的事实。
  return {'BAD_STOCK'}
end

if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'ALREADY'}
end

-- 原子执行不等于失败回滚：先设标记后 INCRBY 仍有部分执行风险，尚未修复。
redis.call('SET', KEYS[2], '1', 'EX', tonumber(ARGV[2]))
redis.call('INCRBY', KEYS[1], ARGV[1])
return {'OK'}
