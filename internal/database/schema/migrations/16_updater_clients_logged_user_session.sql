-- Session state of logged_user, reported by the X-EMLy-LoggedUserState and
-- X-EMLy-LoggedUserDisconnectedAt headers.
--
-- logged_user alone cannot tell someone sitting at the machine from an RDP
-- session left behind by a closed viewer window: the updater reports the
-- owner of a disconnected session too, because for inventory "whose machine
-- is this" is what it answers. logged_user_state says which it is -
-- 'active-console', 'active-rdp' or 'disconnected' - and
-- logged_user_disconnected_at, set only for 'disconnected', says since when
-- (UTC, like every other DATETIME here).
--
-- VARCHAR rather than ENUM so a state added by a future updater is a code
-- change, not a migration; the handler only stores the values it knows.
--
-- Both nullable with no default: NULL means this client has never reported a
-- state (an updater too old to send the header), and for logged_user_disconnected_at
-- also that the session is not disconnected.
ALTER TABLE `updater_clients`
    ADD COLUMN `logged_user_state`           VARCHAR(16) NULL AFTER `logged_user`,
    ADD COLUMN `logged_user_disconnected_at` DATETIME    NULL AFTER `logged_user_state`;
