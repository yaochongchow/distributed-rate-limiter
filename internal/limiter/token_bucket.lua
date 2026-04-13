-- KEYS[1] = bucket hash key
-- ARGV[1] = capacity (int)
-- ARGV[2] = refill_per_sec (float)
-- ARGV[3] = now_ms (int)
-- ARGV[4] = ttl_ms (int)
-- ARGV[5] = cost (int)

local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])
local cost = tonumber(ARGV[5])

if capacity == nil or capacity <= 0 or refill == nil or refill <= 0 or cost == nil or cost <= 0 then
  return { 0, 0, -1 }
end

local data = redis.call("HMGET", key, "tokens", "last_ms")
local tokens = tonumber(data[1])
local last_ms = tonumber(data[2])

if tokens == nil then
  tokens = capacity
  last_ms = now_ms
end

local delta = math.max(0, now_ms - last_ms)
local add = (delta / 1000.0) * refill
tokens = math.min(capacity, tokens + add)
last_ms = now_ms

local allowed = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
end

redis.call("HMSET", key, "tokens", tokens, "last_ms", last_ms)
redis.call("PEXPIRE", key, ttl_ms)

local retry_after_ms = 0
if allowed == 0 then
  local need = cost - tokens
  retry_after_ms = math.ceil((need / refill) * 1000.0)
end

return { allowed, math.floor(tokens), retry_after_ms }
