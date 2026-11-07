-- Events EMLy Updaters send on GET /v2/client/ws (CLIENT_WS_PROTOCOL.md §8),
-- written by internal/clientws and pruned (optionally archived to S3) by
-- internal/eventprune after CLIENT_WS_EVENTS_RETENTION_DAYS.
--
-- Kept apart from updater_events on purpose: that table is a narrow, fixed
-- row per manifest check/download feeding the updater_event_hourly rollup,
-- while these are rarer, carry a variable JSON payload and are history, not
-- fleet aggregates. machine.info (state, already folded into
-- updater_clients) and session.changed with changed=false are not stored.
--
-- id is an auto-increment so the prune loop can delete by primary-key range
-- like it does on updater_events. event_id is the envelope's ULID, unique so
-- an event the client re-sends from its offline buffer is stored once.
-- payload is NULL when it exceeded the storage cap (payload_dropped = 1).
-- client_ts is the machine's own clock, informative only, created_at is the
-- server's receive time and the one everything is ordered and pruned by.
CREATE TABLE IF NOT EXISTS `updater_client_ws_events` (
    `id`              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `event_id`        CHAR(26)     NOT NULL,
    `client_id`       INT UNSIGNED NOT NULL,
    `name`            VARCHAR(40)  NOT NULL,
    `payload`         JSON         NULL,
    `payload_dropped` TINYINT(1)   NOT NULL DEFAULT 0,
    `client_ts`       DATETIME(3)  NULL,
    `created_at`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY `uq_event_id` (`event_id`),
    INDEX `idx_client_created` (`client_id`, `created_at`),
    FOREIGN KEY (`client_id`) REFERENCES `updater_clients` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
