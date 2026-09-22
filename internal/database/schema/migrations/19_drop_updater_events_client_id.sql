-- idx_client_id (client_id) is fully subsumed by idx_client_created
-- (client_id, created_at) added in task 18: every lookup that could use the
-- former reads the same client_id prefix from the latter, and the foreign key
-- on client_id is satisfied by that prefix too.
--
-- Same reasoning as task 13: updater_events is insert-heavy, so a redundant
-- secondary index is a write amplified on the hot ingest path, not merely
-- clutter.
--
-- Split from task 18 so this stays independently guarded - a schema that
-- already lacks the index skips this rather than failing startup.
ALTER TABLE `updater_events`
    DROP INDEX `idx_client_id`,
    ALGORITHM=INPLACE, LOCK=NONE;
