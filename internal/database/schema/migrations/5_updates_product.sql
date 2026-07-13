ALTER TABLE `update_releases`
    DROP INDEX `version`,
    ADD COLUMN `product` ENUM('app', 'updater') NOT NULL DEFAULT 'app' AFTER `id`,
    ADD UNIQUE INDEX `idx_product_version` (`product`, `version`),
    DROP INDEX `idx_channel`,
    ADD INDEX `idx_product_channel` (`product`, `channel`);
