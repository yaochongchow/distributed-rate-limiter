-- KEYS[1] = zset key
-- ARGV[1] = limit (int)
-- ARGV[2] = window_ms (int)
-- ARGV[3] = now_ms (int)
-- ARGV[4] = ttl_ms (int)
-- ARGV[5] = cost (int)

local key = KEYS[1]
local seq_key = key .. ":seq"
local limit = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])
local cost = tonumber(ARGV[5])

if limit == nil or limit <= 0 or window_ms == nil or window_ms <= 0 or cost == nil or cost <= 0 then
  return { 0, 0, -1 }
end

local min_score = now_ms - window_ms
redis.call("ZREMRANGEBYSCORE", key, 0, min_score)

local count = tonumber(redis.call("ZCARD", key))
local allowed = 0

if (count + cost) <= limit then
  allowed = 1
  local next_seq = redis.call("INCRBY", seq_key, cost)
  local first_seq = next_seq - cost + 1
  for i = 0, cost - 1 do
    local member = tostring(now_ms) .. "-" .. tostring(first_seq + i)
    redis.call("ZADD", key, now_ms, member)
  end
  redis.call("PEXPIRE", seq_key, ttl_ms)
end

redis.call("PEXPIRE", key, ttl_ms)

local remaining = math.max(0, limit - count - (allowed == 1 and cost or 0))
local retry_after_ms = 0
if allowed == 0 then
  local expire_needed = count + cost - limit
  local target = redis.call("ZRANGE", key, expire_needed - 1, expire_needed - 1, "WITHSCORES")
  if target[2] ~= nil then
    retry_after_ms = math.max(0, tonumber(target[2]) + window_ms - now_ms)
  else
    retry_after_ms = window_ms
  end
end

return { allowed, remaining, retry_after_ms }
