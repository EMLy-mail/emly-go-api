-- Scope update_releases by product. Every existing row is an EMLy release.
--
-- The version is now unique per product rather than globally (two products
-- can both ship a 1.0.0), and the stable/beta lookups the manifest and the
-- flag-clearing statements run are always "within one product", hence the
-- composite indexes replacing the single-column ones.
ALTER TABLE `update_releases`
    ADD COLUMN `product` VARCHAR(20) NOT NULL DEFAULT 'emly' AFTER `id`,
    DROP INDEX `version`,
    DROP INDEX `idx_is_stable`,
    DROP INDEX `idx_is_beta`,
    ADD UNIQUE KEY `uq_product_version` (`product`, `version`),
    ADD INDEX `idx_product_stable` (`product`, `is_stable`),
    ADD INDEX `idx_product_beta` (`product`, `is_beta`);
