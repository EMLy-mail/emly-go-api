CREATE TABLE IF NOT EXISTS `updater_releases` (
    `id`                INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `version`           VARCHAR(20)     NOT NULL UNIQUE,
    `is_current`        TINYINT(1)      NOT NULL DEFAULT 0,
    `download_filename` VARCHAR(255)    NOT NULL,
    `sha256_checksum`   CHAR(64)        NOT NULL,
    `file_size`         BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `notes_it`          TEXT            NULL,
    `notes_en`          TEXT            NULL,
    `published_at`      TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `created_at`        TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_is_current` (`is_current`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
