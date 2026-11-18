local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'NOT_READY'}
end

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

local streamLen = redis.call('XLEN', KEYS[3])
if streamLen >= tonumber(ARGV[9]) then
  return {'QUEUE_FULL'}
end

redis.call('DECRBY', KEYS[1], qty)
local messageID = redis.call('XADD', KEYS[3], '*',
  'reservationId', ARGV[4],
  'idempotencyKey', ARGV[2],
  'userId', ARGV[1],
  'itemId', ARGV[5],
  'quantity', ARGV[6],
  'payloadHash', ARGV[3],
  'acceptedAt', ARGV[7])

redis.call('HSET', KEYS[2],
  'status', 'PROCESSING',
  'reservation_id', ARGV[4],
  'payload_hash', ARGV[3],
  'accepted_at', ARGV[7],
  'updated_at', ARGV[7])
redis.call('PEXPIRE', KEYS[2], tonumber(ARGV[8]))

return {'ACCEPTED', tostring(available - qty), messageID}
