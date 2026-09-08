CREATE TABLE IF NOT EXISTS `remote_config_revisions` (
    `revision`       INT UNSIGNED    NOT NULL PRIMARY KEY,
    `schema_version` INT UNSIGNED    NOT NULL,
    `status`         ENUM('draft', 'published', 'superseded') NOT NULL DEFAULT 'draft',
    `document`       MEDIUMTEXT      NOT NULL,
    `etag`           CHAR(64)        NOT NULL,
    `notes`          TEXT            NULL,
    `created_by`     VARCHAR(255)    NULL,
    `based_on`       INT UNSIGNED    NULL,
    `generated_at`   TIMESTAMP       NOT NULL,
    `published_at`   TIMESTAMP       NULL,
    `created_at`     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
