-- Repairs update_releases on a database that ran the v3 branch's
-- 5_updates_product.sql before migration 23 existed. That migration added
-- product as ENUM('app', 'updater') DEFAULT 'app', so 23 (which only runs
-- while the column is missing) was skipped: every release kept product =
-- 'app', invisible to the emly queries, and the ENUM refuses 'emly' and any
-- new product slug.
--
-- Runs only while product is not VARCHAR(20), the type 23 creates.
-- Type first, since an ENUM cannot hold 'emly'. The v3 'app' was EMLy.
-- Rows with product = 'updater' are left alone: the Agent's own releases
-- live in updater_releases (migration 7), so such rows are v3 leftovers
-- for a person to look at, not something to rename blindly.
ALTER TABLE `update_releases`
    MODIFY `product` VARCHAR(20) NOT NULL DEFAULT 'emly';

UPDATE `update_releases` SET `product` = 'emly' WHERE `product` = 'app';
