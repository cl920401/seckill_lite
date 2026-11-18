local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])

if not rate or rate <= 0 or not burst or burst <= 0 or not cost or cost <= 0 then
  return {0, 0, 1000}
end

local values = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(values[1])
local ts = tonumber(values[2])
if not tokens or not ts then
  tokens = burst
  ts = now
end

local elapsed = math.max(0, now - ts) / 1000
tokens = math.min(burst, tokens + elapsed * rate)

if tokens < cost then
  local retry = math.ceil((cost - tokens) / rate * 1000)
  redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
  redis.call('PEXPIRE', KEYS[1], ttl)
  return {0, math.floor(tokens), retry}
end

tokens = tokens - cost
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], ttl)
return {1, math.floor(tokens), 0}
