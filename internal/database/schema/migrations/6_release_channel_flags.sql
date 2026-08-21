ALTER TABLE `update_releases`
    ADD COLUMN `is_stable` TINYINT(1) NOT NULL DEFAULT 0 AFTER `channel`,
    ADD COLUMN `is_beta`   TINYINT(1) NOT NULL DEFAULT 0 AFTER `is_stable`;

UPDATE `update_releases` SET `is_stable` = 1 WHERE `channel` = 'stable';

UPDATE `update_releases` SET `is_beta` = 1 WHERE `channel` = 'beta';

ALTER TABLE `update_releases`
    DROP COLUMN `channel`,
    ADD INDEX `idx_is_stable` (`is_stable`),
    ADD INDEX `idx_is_beta` (`is_beta`);
