-- Index for the one-client history GET /v2/stats/clients/{id} reads:
--   SELECT * FROM updater_events WHERE client_id = ? ORDER BY created_at DESC
--    LIMIT 50
--
-- idx_client_id (client_id) narrows to the right client but hands the rows
-- back in primary-key order, so MySQL reads every event that client ever
-- produced and sorts the lot to return 50 of them. That set only grows: one
-- row per manifest check per cycle put ~2500 rows behind each machine of a
-- ~200-client fleet by the time the table reached 500k rows, and the sort is
-- paid on every dashboard click.
--
-- With created_at as the second column the same query is a backwards range
-- scan that stops after 50 entries, independent of how much history the
-- client has.
--
-- The foreign key on client_id keeps working across this: client_id is the
-- leftmost column of the new index, which is all InnoDB asks of a supporting
-- index, so task 19 can drop the now-redundant idx_client_id.
ALTER TABLE `updater_events`
    ADD INDEX `idx_client_created` (`client_id`, `created_at`),
    ALGORITHM=INPLACE, LOCK=NONE;
