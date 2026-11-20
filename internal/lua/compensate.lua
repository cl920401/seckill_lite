-- 预约补偿：仅供 DB 已持久拒绝/确认多余预扣、且补偿未 DONE 的预约使用。
-- KEYS：1=商品库存；2=compensated:{reservationId}；3=该用户的幂等请求 Hash。
-- ARGV：1=reservationId；2=回补数量；3=拒绝原因；4=更新时间（毫秒）。
-- 标记不设 TTL，用于覆盖“Redis 已回补但 DB 尚未记 DONE”的重投窗口；
-- 它不能抵御 Redis 数据丢失。DB 已 DONE 的重投应直接 ACK，不再调用本脚本。
-- 库存缺失时不创建库存或标记，避免无依据重建后放行更多请求。
local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'MISS'}
end
-- 注意：tonumber 仅验证能否转成 Lua 数字，不是 Redis int64 整数/溢出校验。
-- 小数等值仍可能穿过此判断，因此下面“先标记、后 INCRBY”的错误窗口尚存在。
if not tonumber(redis.call('GET', KEYS[1])) then
  -- 对不能转成数字的库存提前退出，避免留下误导后续重试的标记。
  return {'BAD_STOCK'}
end

if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'ALREADY'}
end

-- Lua 不回滚运行期错误：若 INCRBY 失败，marker 仍存在，重试可能误报 ALREADY。
-- 这是现有实现的限制，不能把 ALREADY 无条件等同于已核实的库存回补。
redis.call('SET', KEYS[2], '1')
redis.call('INCRBY', KEYS[1], ARGV[2])

-- 只修改仍属于本预约且未成功的缓存，避免额外预约的拒绝覆盖获胜订单结果。
if redis.call('EXISTS', KEYS[3]) == 1 then
  local rid = redis.call('HGET', KEYS[3], 'reservation_id')
  local status = redis.call('HGET', KEYS[3], 'status')
  if rid == ARGV[1] and status ~= 'SUCCEEDED' then
    redis.call('HSET', KEYS[3], 'status', 'REJECTED', 'reason', ARGV[3], 'updated_at', ARGV[4])
  end
end
return {'OK'}
