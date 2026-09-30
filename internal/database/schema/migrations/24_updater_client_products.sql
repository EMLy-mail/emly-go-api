-- Which products each machine has installed, and at which version.
--
-- updater_clients.emly_version only ever described one product. This is its
-- per-product generalization, fed by the X-EMLy-InstalledProducts header and
-- the installed_products field of the /v2/client/ws identity message
-- (updaterclient.Upsert). emly_version stays, still filled from
-- X-EMLy-AppVersion, for dashboards and updaters that predate this table.
--
-- A snapshot, like logged_user: one row per (client, product), overwritten on
-- every report, deleted when a full inventory no longer lists the product.
CREATE TABLE IF NOT EXISTS `updater_client_products` (
    `client_id`  INT UNSIGNED NOT NULL,
    `product`    VARCHAR(20)  NOT NULL,
    `version`    VARCHAR(20)  NOT NULL,
    `updated_at` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`client_id`, `product`),
    INDEX `idx_product_version` (`product`, `version`),
    FOREIGN KEY (`client_id`) REFERENCES `updater_clients` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Seed from what is already known, so the per-product view is not empty
-- until every machine has checked in again.
INSERT IGNORE INTO `updater_client_products` (`client_id`, `product`, `version`)
SELECT `id`, 'emly', `emly_version`
  FROM `updater_clients`
 WHERE `emly_version` IS NOT NULL AND `emly_version` <> '';
