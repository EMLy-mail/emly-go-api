ALTER TABLE `updater_clients`
    ADD COLUMN `config_revision`   INT UNSIGNED NULL AFTER `updater_version`,
    ADD COLUMN `config_fetched_at` TIMESTAMP    NULL AFTER `config_revision`,
    ADD INDEX  `idx_config_revision` (`config_revision`);
