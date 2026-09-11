-- Permanent, operator-set blocks, distinct from the rate limiter's automatic
-- and time-boxed ones (those live in memory and expire on their own).
--
-- ban_type/value is the whole rule: one row bans one identifier. Banning a
-- machine by both its hwid and its hostname is two rows, not one row with two
-- columns - it keeps the matcher a map lookup per type and makes "why is this
-- client blocked" answerable by pointing at a single row.
--
-- The unique key is what makes a repeated ban idempotent rather than a
-- duplicate. Hostnames are stored lower-cased by the handler so that key is
-- also the case-insensitive one the matcher needs.
CREATE TABLE IF NOT EXISTS `bans` (
    `id`         INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `ban_type`   ENUM('ip', 'hwid', 'hostname') NOT NULL,
    `value`      VARCHAR(255) NOT NULL,
    `reason`     VARCHAR(500) NULL,
    `created_by` VARCHAR(255) NULL,
    `created_at` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY `uniq_ban` (`ban_type`, `value`),
    INDEX `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
