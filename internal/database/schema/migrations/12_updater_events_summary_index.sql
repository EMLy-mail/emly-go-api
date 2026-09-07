-- Covering index for the /v2/stats/summary event rollup:
--   SELECT event_type, COUNT(*) FROM updater_events
--    WHERE created_at >= NOW() - INTERVAL 24 HOUR AND product = ?
--    GROUP BY event_type
--
-- Neither pre-existing index serves it. idx_event_type_created (event_type,
-- created_at) is what the optimizer actually picked - it hands over the
-- GROUP BY already ordered, but has no product column, so it pays with a full
-- index scan plus a clustered-index lookup per candidate row (measured:
-- 120753 rows scanned to keep 17865, 282ms). idx_product_created (product,
-- created_at) can range-scan but leaves event_type unavailable, forcing a
-- row lookup per match and a temporary table for the grouping.
--
-- With product first, event_type second and created_at last, the query
-- becomes one range scan per event_type inside the requested product, read
-- entirely from the index and already grouped.
ALTER TABLE `updater_events`
    ADD INDEX `idx_product_event_created` (`product`, `event_type`, `created_at`),
    ALGORITHM=INPLACE, LOCK=NONE;
