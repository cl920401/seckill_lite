-- 令牌桶：在一次脚本内完成补充和扣除，避免并发读改写造成超额放行。
-- KEYS：1=限流桶 Hash（全局入口、全局查询或单用户新单各使用独立 Key）。
-- ARGV：1=每秒补充速率；2=桶容量；3=调用方当前时间（毫秒）；
--       4=本次消耗；5=桶 TTL（毫秒）。返回：是否放行、剩余整令牌、建议等待毫秒。
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

-- 按应用时间补充，容量限制短时突发；max(0) 仅避免时钟回退产生负补充。
local elapsed = math.max(0, now - ts) / 1000
tokens = math.min(burst, tokens + elapsed * rate)

-- 拒绝也保存时间和余额；TTL 清理闲置桶，之后首次访问按满桶重新开始。
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
