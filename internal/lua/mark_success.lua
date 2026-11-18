if redis.call('EXISTS', KEYS[1]) == 0 then
  return 0
end
local rid = redis.call('HGET', KEYS[1], 'reservation_id')
local status = redis.call('HGET', KEYS[1], 'status')
if rid ~= ARGV[1] or status == 'REJECTED' then
  return 0
end
redis.call('HSET', KEYS[1], 'status', 'SUCCEEDED', 'order_no', ARGV[2], 'updated_at', ARGV[3])
return 1
