-- Version of the EMLy app itself, reported by the X-EMLy-AppVersion header.
--
-- updater_version already records the version of the EMLy Updater, taken from
-- its User-Agent. That is the version of the thing doing the talking, not of
-- the app it maintains: a fleet can sit on one current updater while the EMLy
-- installs behind it lag several releases. emly_version is that second number,
-- so "who is still on the old EMLy" is answerable without joining events.
--
-- VARCHAR(20) matches updater_clients.updater_version and updater_events.version;
-- the handler truncates to the same width so an over-long header cannot fail
-- the whole telemetry upsert.
--
-- Nullable with no default: NULL means this client has never reported one - an
-- updater too old to send the header, or one that ran before EMLy was installed.
ALTER TABLE `updater_clients`
    ADD COLUMN `emly_version` VARCHAR(20) NULL AFTER `updater_version`;
