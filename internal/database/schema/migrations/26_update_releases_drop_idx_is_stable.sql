-- Replaced by idx_product_stable: the stable lookup is always within one
-- product. Migration 23 drops it, but a database that skipped 23 still has it.
ALTER TABLE `update_releases` DROP INDEX `idx_is_stable`;
