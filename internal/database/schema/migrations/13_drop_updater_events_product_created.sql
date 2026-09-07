-- idx_product_created (product, created_at) is fully subsumed by
-- idx_product_event_created (product, event_type, created_at) added in task
-- 12: every query that could use the former reads the same product prefix
-- from the latter, covered. The only remaining product-without-event_type
-- query is /v2/stats/events, which groups by event_type anyway and so has to
-- walk the whole prefix either way.
--
-- Dropping it is not just tidiness: updater_events is insert-heavy (one row
-- per manifest check per client), and every redundant secondary index is a
-- write amplified on that hot path.
--
-- Split from task 12 so this stays independently guarded - a schema that
-- already lacks the index skips this rather than failing startup.
ALTER TABLE `updater_events`
    DROP INDEX `idx_product_created`,
    ALGORITHM=INPLACE, LOCK=NONE;
