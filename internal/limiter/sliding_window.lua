-- KEYS[1] = zset key
-- ARGV[1] = limit (int)
-- ARGV[2] = window_ms (int)
-- ARGV[3] = now_ms (int)
-- ARGV[4] = ttl_ms (int)
-- ARGV[5] = cost (int)

local key = KEYS[1]
local limit = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])
local cost = tonumber(ARGV[5])

local min_score = now_ms - window_ms
redis.call("ZREMRANGEBYSCORE", key, 0, min_score)

local count = tonumber(redis.call("ZCARD", key))
local allowed = 0

if (count + cost) <= limit then
  allowed = 1
  for i = 1, cost do
    local member = tostring(now_ms) .. "-" .. tostring(math.random(1000000))
    redis.call("ZADD", key, now_ms, member)
  end
end

redis.call("PEXPIRE", key, ttl_ms)

local remaining = math.max(0, limit - count - (allowed == 1 and cost or 0))
local retry_after_ms = 0
if allowed == 0 then
  retry_after_ms = window_ms
end

return { allowed, remaining, retry_after_ms }