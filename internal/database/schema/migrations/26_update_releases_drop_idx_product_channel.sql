-- v3 leftover: an index on (product, channel) that lost channel when
-- migration 6 dropped the column, leaving a prefix of the unique
-- (product, version) key.
ALTER TABLE `update_releases` DROP INDEX `idx_product_channel`;
