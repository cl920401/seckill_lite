local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'MISS'}
end
if not tonumber(redis.call('GET', KEYS[1])) then
  -- Do not set the marker if INCRBY cannot succeed; a marker without a
  -- successful increment would mislead later retries/manual recovery.
  return {'BAD_STOCK'}
end

if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'ALREADY'}
end

redis.call('SET', KEYS[2], '1', 'EX', tonumber(ARGV[2]))
redis.call('INCRBY', KEYS[1], ARGV[1])
return {'OK'}
