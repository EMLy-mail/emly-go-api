CREATE TABLE IF NOT EXISTS `update_releases` (
    `id`                   INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `version`              VARCHAR(20)  NOT NULL UNIQUE,
    `channel`              ENUM('stable', 'beta', 'archived') NOT NULL DEFAULT 'archived',
    `download_filename`    VARCHAR(255) NOT NULL,
    `sha256_checksum`      CHAR(64)     NOT NULL DEFAULT '',
    `short_note`           TEXT         NOT NULL,
    `severity_type`        ENUM('none', 'security', 'bugfix', 'feature') NOT NULL DEFAULT 'none',
    `description_en`       TEXT         NULL,
    `description_it`       TEXT         NULL,
    `is_critical`          TINYINT(1)   NOT NULL DEFAULT 0,
    `min_required_version` VARCHAR(20)  NULL,
    `released_at`          TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `created_at`           TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_channel` (`channel`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
