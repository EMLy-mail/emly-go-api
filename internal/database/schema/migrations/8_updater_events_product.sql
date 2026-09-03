ALTER TABLE `updater_events`
    ADD COLUMN `product` VARCHAR(20) NOT NULL DEFAULT 'emly' AFTER `event_type`,
    ADD INDEX `idx_product_created` (`product`, `created_at`);
