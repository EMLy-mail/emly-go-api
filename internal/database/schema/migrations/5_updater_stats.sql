CREATE TABLE IF NOT EXISTS `updater_clients` (
    `id`               INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `hostname`         VARCHAR(255) NOT NULL,
    `ad_domain`        VARCHAR(255) NOT NULL DEFAULT '',
    `updater_version`  VARCHAR(20)  NULL,
    `contact`          VARCHAR(255) NULL,
    `last_ip`          VARCHAR(45)  NULL,
    `first_seen_at`    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `last_seen_at`     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY `uniq_client` (`hostname`, `ad_domain`),
    INDEX `idx_last_seen` (`last_seen_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `updater_events` (
    `id`          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `client_id`   INT UNSIGNED NOT NULL,
    `event_type`  ENUM('manifest_check', 'download') NOT NULL,
    `version`     VARCHAR(20) NULL,
    `ip_address`  VARCHAR(45) NULL,
    `created_at`  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_client_id` (`client_id`),
    INDEX `idx_event_type_created` (`event_type`, `created_at`),
    FOREIGN KEY (`client_id`) REFERENCES `updater_clients` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
