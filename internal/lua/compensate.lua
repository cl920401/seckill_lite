local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'MISS'}
end
if not tonumber(redis.call('GET', KEYS[1])) then
  -- Do not set the marker: INCRBY would fail and a retry would report ALREADY
  -- even though stock was never restored.
  return {'BAD_STOCK'}
end

if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'ALREADY'}
end

redis.call('SET', KEYS[2], '1')
redis.call('INCRBY', KEYS[1], ARGV[2])

if redis.call('EXISTS', KEYS[3]) == 1 then
  local rid = redis.call('HGET', KEYS[3], 'reservation_id')
  local status = redis.call('HGET', KEYS[3], 'status')
  if rid == ARGV[1] and status ~= 'SUCCEEDED' then
    redis.call('HSET', KEYS[3], 'status', 'REJECTED', 'reason', ARGV[3], 'updated_at', ARGV[4])
  end
end
return {'OK'}
