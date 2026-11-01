-- rate_limit_hwid was created by init.sql for a per-HWID rate limit that the
-- API never ended up using: rate limiting is per IP and in memory
-- (internal/middleware/ratelimit.ban.go), and no Go code reads or writes this
-- table. Dropped so the schema only holds tables something owns.
DROP TABLE IF EXISTS `rate_limit_hwid`;
