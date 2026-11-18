local stype = redis.call('TYPE', KEYS[1])
if type(stype) == 'table' then stype = stype['ok'] end
if stype ~= 'string' then
  return {'MISS'}
end

if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'ALREADY'}
end

redis.call('SET', KEYS[2], '1', 'EX', tonumber(ARGV[2]))
redis.call('INCRBY', KEYS[1], ARGV[1])
return {'OK'}
