ALTER TABLE `update_releases`
    ADD COLUMN `critical_version` VARCHAR(20) NULL AFTER `is_critical`;
