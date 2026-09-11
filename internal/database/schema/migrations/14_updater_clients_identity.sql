-- Machine identity reported by the EMLy Updater's X-EMLy-* headers.
--
-- logged_user is the interactive user (console or RDP) at the time of the
-- last sighting - a snapshot, not history: it is overwritten on every
-- request that carries the header, and the row's last_seen_at says how old
-- it is. serial/product are the firmware's chassis serial and vendor
-- product/SKU number, which identify the box to its vendor (warranty,
-- procurement) the way hwid identifies it to us.
--
-- All three are nullable with no default: NULL means "this client has never
-- reported one" - an updater too old to send the header, a machine with
-- nobody logged on, or firmware that only carries OEM placeholders.
ALTER TABLE `updater_clients`
    ADD COLUMN `logged_user` VARCHAR(255) NULL AFTER `ad_domain`,
    ADD COLUMN `serial`      VARCHAR(128) NULL AFTER `logged_user`,
    ADD COLUMN `product`     VARCHAR(128) NULL AFTER `serial`,
    ADD INDEX  `idx_logged_user` (`logged_user`),
    ADD INDEX  `idx_serial` (`serial`);
