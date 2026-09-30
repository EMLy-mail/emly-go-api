-- The index migration 23 creates, for a database that skipped 23.
ALTER TABLE `update_releases` ADD INDEX `idx_product_beta` (`product`, `is_beta`);
