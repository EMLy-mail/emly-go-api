-- Log files machines sent back through the client channel's log.send_latest
-- command (CLIENT_WS_PROTOCOL.md §7.7), kept by internal/clientlogs.
--
-- One row per command: id is the command id (a ULID), so the dashboard finds
-- a log from the command that fetched it. The gzip lives in the API file
-- bucket (s3_key) when USE_S3_API_FILE_STORAGE is on, in `content`
-- otherwise - never both.
--
-- No foreign key to updater_clients on purpose: a cascade would delete the
-- rows of a deleted machine and orphan their bucket objects. The prune loop
-- removes those rows itself, object first.
--
-- hostname and updater_version are snapshots taken when the log arrived: a
-- machine can be renamed or updated afterwards.
CREATE TABLE IF NOT EXISTS `updater_client_logs` (
    `id`              CHAR(26)     NOT NULL,
    `client_id`       INT UNSIGNED NOT NULL,
    `issued_by`       VARCHAR(255) NULL,
    `file_name`       VARCHAR(64)  NOT NULL,
    `size_bytes`      BIGINT       NOT NULL,
    `offset_bytes`    BIGINT       NOT NULL,
    `included_bytes`  BIGINT       NOT NULL,
    `truncated`       TINYINT(1)   NOT NULL,
    `mismatch`        TINYINT(1)   NOT NULL DEFAULT 0,
    `log_level`       VARCHAR(10)  NULL,
    `modified_at`     DATETIME     NULL,
    `collected_at`    DATETIME     NULL,
    `received_at`     DATETIME     NOT NULL,
    `gzip_bytes`      INT UNSIGNED NOT NULL,
    `updater_version` VARCHAR(20)  NULL,
    `hostname`        VARCHAR(255) NULL,
    `s3_key`          VARCHAR(255) NULL,
    `content`         MEDIUMBLOB   NULL,
    PRIMARY KEY (`id`),
    INDEX `idx_client_received` (`client_id`, `received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
