-- 下单准入：预扣 Redis 库存并投递预约，订单仍须由 Worker 在 MySQL 中创建。
-- KEYS：1=商品库存；2=用户维度的幂等请求 Hash；3=订单预约 Stream。
-- ARGV：1=userId；2=idempotencyKey；3=payloadHash；4=reservationId；5=itemId；
--       6=quantity；7=acceptedAt（毫秒）；8=请求状态 TTL（毫秒）；9=Stream 条目上限。
-- 脚本执行期间不会被其他命令穿插，但运行期报错不会回滚此前写入。
-- 所以脚本报错/响应超时不能证明“未受理”，调用方须用原幂等键确认结果。

-- 当前实现先检查库存键类型；缺失或错误类型都保持未就绪，不从 DB 猜测重建。
local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'NOT_READY'}
end

-- 同键同指纹复用原预约，不再次预扣；不同指纹拒绝，持久结果由服务层回查 DB。
if redis.call('EXISTS', KEYS[2]) == 1 then
  local rtype = redis.call('TYPE', KEYS[2])
  if type(rtype) == 'table' then rtype = rtype['ok'] end
  if rtype ~= 'hash' then
    return {'NOT_READY'}
  end
  local stored = redis.call('HGET', KEYS[2], 'payload_hash')
  if not stored or stored ~= ARGV[3] then
    return {'CONFLICT'}
  end
  local status = redis.call('HGET', KEYS[2], 'status') or 'PROCESSING'
  local orderNo = redis.call('HGET', KEYS[2], 'order_no') or ''
  return {'DUPLICATE', status, orderNo}
end

local stock = redis.call('GET', KEYS[1])
if not stock then
  return {'NOT_READY'}
end
local available = tonumber(stock)
local qty = tonumber(ARGV[6])
if not available or not qty or qty <= 0 then
  return {'NOT_READY'}
end
if available < qty then
  return {'SOLD_OUT'}
end

-- XLEN 包括已 ACK 的历史条目，并非待处理数；ACK 不会自动释放这里的容量。
-- 在同一脚本内检查上限和 XADD，避免并发请求同时越过容量检查。
local streamLen = redis.call('XLEN', KEYS[3])
if streamLen >= tonumber(ARGV[9]) then
  return {'QUEUE_FULL'}
end

-- 每次实际预扣对应一个 reservationId；重投该消息仍使用原 ID。
-- 此处及后续 XADD/HSET 任一步报错都可能留下部分写入，不能按数据库事务理解。
redis.call('DECRBY', KEYS[1], qty)
local messageID = redis.call('XADD', KEYS[3], '*',
  'reservationId', ARGV[4],
  'idempotencyKey', ARGV[2],
  'userId', ARGV[1],
  'itemId', ARGV[5],
  'quantity', ARGV[6],
  'payloadHash', ARGV[3],
  'acceptedAt', ARGV[7])

-- PROCESSING 只表示已进入异步流程；TTL 过期不表示失败，也不能成为回补依据。
redis.call('HSET', KEYS[2],
  'status', 'PROCESSING',
  'reservation_id', ARGV[4],
  'payload_hash', ARGV[3],
  'accepted_at', ARGV[7],
  'updated_at', ARGV[7])
redis.call('PEXPIRE', KEYS[2], tonumber(ARGV[8]))

return {'ACCEPTED', tostring(available - qty), messageID}
