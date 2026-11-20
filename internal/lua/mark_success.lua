-- 成功订单的尽力缓存回填，MySQL 已提交结果才是事实来源。
-- KEYS：1=用户维度的幂等请求 Hash。
-- ARGV：1=reservationId；2=orderNo；3=更新时间（毫秒）。
-- 缓存过期时不重建，也不延长 TTL；返回 0 不否定 DB 中的成功订单。
if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
local rid = redis.call('HGET', KEYS[1], 'reservation_id')
local status = redis.call('HGET', KEYS[1], 'status')
-- 只允许原预约推进到成功，防止迟到 Worker 覆盖新预约或已拒绝的缓存。
if rid ~= ARGV[1] or status == 'REJECTED' then
  return 0
end
redis.call('HSET', KEYS[1], 'status', 'SUCCEEDED', 'order_no', ARGV[2], 'updated_at', ARGV[3])
return 1
