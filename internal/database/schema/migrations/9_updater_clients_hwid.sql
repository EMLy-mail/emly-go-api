ALTER TABLE `updater_clients`
    ADD COLUMN `hwid` VARCHAR(64) NULL AFTER `id`,
    ADD UNIQUE KEY `uniq_hwid` (`hwid`);
