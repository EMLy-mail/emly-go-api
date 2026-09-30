-- Registry of the software products the updates surface distributes.
--
-- EMLy used to be the only one, implied by every query on update_releases.
-- Each product now has its own releases, its own stable/beta/critical slots
-- and its own manifest under /v2/updates/{slug}/..., while the legacy
-- /v2/updates/manifest and /v2/updates/releases/... keep answering for emly.
--
-- slug is VARCHAR(20) to match updater_events.product and
-- updater_event_hourly.product: it is the value written there, so a slug that
-- fits here always fits in the telemetry.
--
-- s3_prefix NULL means "derive it": emly keeps S3_UPDATES_PREFIX (where its
-- installers already are), every other product gets S3_UPDATES_PREFIX/<slug>.
-- A migration cannot read the environment, so the default is resolved in Go
-- (productreg.S3Prefix), not stored here.
CREATE TABLE IF NOT EXISTS `products` (
    `slug`       VARCHAR(20)  NOT NULL PRIMARY KEY,
    `name`       VARCHAR(100) NOT NULL,
    `s3_prefix`  VARCHAR(255) NULL,
    `enabled`    TINYINT(1)   NOT NULL DEFAULT 1,
    `created_at` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT IGNORE INTO `products` (`slug`, `name`, `s3_prefix`, `enabled`) VALUES ('emly', 'EMLy', NULL, 1);
