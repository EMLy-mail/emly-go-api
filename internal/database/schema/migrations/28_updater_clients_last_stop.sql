-- How each machine's EMLy Updater service last stopped, from the client
-- channel's service.stopped event or, when that never arrived, from the
-- last_stop of the next service.started (CLIENT_WS_PROTOCOL.md §8.6, §8.7).
--
-- The per-client event ring in internal/clienthub is in memory, holds 50
-- entries and is pruned after 24h: a machine switched off on Friday evening
-- has lost its service.stopped by Monday, and so has every machine after an
-- API restart. These columns are what survives.
--
-- Last value only, not a history. VARCHAR rather than ENUM so a reason added
-- by a future updater is stored as received instead of rejected.
-- last_stop_at is NULL for reason 'unexpected' (a crash leaves no time), and
-- all three are NULL for a machine that has never reported a stop.
--
-- No semicolons in these comments: the migrator splits the file on every
-- one of them, comments included.
ALTER TABLE `updater_clients`
    ADD COLUMN `last_stop_reason`   VARCHAR(20) NULL,
    ADD COLUMN `last_stop_category` VARCHAR(20) NULL AFTER `last_stop_reason`,
    ADD COLUMN `last_stop_at`       DATETIME    NULL AFTER `last_stop_category`;
