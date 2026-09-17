-- Windows version of the machine, reported by the X-EMLy-OSVersion header.
--
-- One human-readable string built by the updater from the registry, e.g.
-- 'Windows 11 24H2 Professional (Build 26100.4652)' - product name, display
-- version, edition and the full build with its UBR. It is stored as sent
-- rather than split into columns: the fleet view reads it, nothing computes
-- on it, and the shape of that string is the updater's business.
--
-- 128 characters is roughly three times the longest string the current
-- builder can produce, and the handler truncates to the same width, so an
-- unusually verbose ProductName cannot fail the upsert.
--
-- No semicolons in these comments: the migrator splits the file on every one
-- of them, comments included.
--
-- Nullable with no default: NULL means this client has never reported one -
-- an updater too old to send the header, or a registry read that failed.
ALTER TABLE `updater_clients`
    ADD COLUMN `os_version` VARCHAR(128) NULL AFTER `product`;
