-- Token bucket, run atomically by Redis.
-- KEYS[1] = bucket key
-- ARGV[1] = refill rate (tokens per second)
-- ARGV[2] = capacity (burst)
-- Returns {allowed (1/0), retry_after_ms}
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])

-- Use Redis's clock, not the caller's, so pods with skewed clocks agree.
local t = redis.call('TIME')
local now = t[1] * 1000 + math.floor(t[2] / 1000)

local state = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then
  tokens = burst
  ts = now
end

-- Refill for the time elapsed since the last request, capped at capacity.
tokens = math.min(burst, tokens + (now - ts) / 1000 * rate)

local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) / rate * 1000)
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
-- Expire idle buckets once they would be full again anyway.
redis.call('PEXPIRE', key, math.ceil(burst / rate * 1000) + 1000)
return {allowed, retry}
