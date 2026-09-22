-- Hourly pre-aggregation of updater_events, so the two dashboard aggregates
-- stop re-scanning raw rows.
--
-- What they cost against the raw table:
--   * GET /v2/stats/summary rolls up 24h of events by type. One index range
--     scan per event_type, but over every row of the last day - tens of
--     thousands at a ~200-machine fleet checking in every few minutes.
--   * GET /v2/stats/events groups 30 days by DATE(created_at). The grouping
--     expression is not indexable at all, so it is a scan of the whole range
--     plus a temporary table and a sort - ~500k rows, every call.
-- And both ran again for every ingested event, for every open
-- /v2/stats/stream connection (see internal/handlers/stats_stream.route.go).
--
-- Against this table the same answers are 48 rows and 720 rows: the summary
-- sums 24 hours x 2 event types, the 30-day daily chart groups 720 hourly
-- rows by DATE(bucket_hour). Hour is the finest bucket the API exposes, so
-- nothing the dashboard can ask for needs the raw rows.
--
-- The counter is incremented in the same transaction as the event insert
-- (recordUpdaterEvent), never by a periodic job: the number a dashboard reads
-- is therefore as live as the event itself, which is the whole point of
-- keeping /v2/stats/stream real-time while taking these queries off the raw
-- table.
--
-- bucket_hour is DATETIME, not TIMESTAMP, and is computed with the same
-- session-time-zone expression the old raw-row queries used
-- (DATE_FORMAT(..., '%Y-%m-%d %H:00:00')), so the buckets land exactly where
-- the previous DATE(created_at)/DATE_FORMAT grouping put them and the charts
-- do not shift under the dashboard when this ships.
--
-- product is VARCHAR, matching updater_events.product, and event_type repeats
-- its ENUM: a new event type or product has to be added here too, or its
-- rows are counted nowhere.
CREATE TABLE IF NOT EXISTS `updater_event_hourly` (
    `bucket_hour` DATETIME NOT NULL,
    `product`     VARCHAR(20) NOT NULL,
    `event_type`  ENUM('manifest_check', 'download') NOT NULL,
    `count`       BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (`bucket_hour`, `product`, `event_type`),
    INDEX `idx_product_bucket` (`product`, `bucket_hour`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Backfill from the history already on disk, so the charts do not start empty
-- and keep reading the same numbers across the deploy. One full scan of
-- updater_events, once, at startup before the router is built.
--
-- Written idempotently on purpose: this task is guarded by
-- table_not_exists, so a failure here leaves a created-but-empty table that
-- the next startup would skip. If that ever happens, this exact statement is
-- the repair - run it by hand and the counters come back consistent.
INSERT INTO `updater_event_hourly` (`bucket_hour`, `product`, `event_type`, `count`)
SELECT DATE_FORMAT(`created_at`, '%Y-%m-%d %H:00:00'), `product`, `event_type`, COUNT(*)
  FROM `updater_events`
 GROUP BY 1, 2, 3
    ON DUPLICATE KEY UPDATE `count` = VALUES(`count`);
