-- SSO (OIDC) users. They are created on first login from the identity
-- provider and never have a usable password: auth_provider tells them apart
-- from local accounts, external_id is the provider's stable `sub` claim.
-- 'owner' is the top role, granted only through the provider's groups.
ALTER TABLE `user` MODIFY COLUMN `role` ENUM('owner', 'admin', 'user') NOT NULL DEFAULT 'user';
ALTER TABLE `user` ADD COLUMN `auth_provider` ENUM('local', 'oidc') NOT NULL DEFAULT 'local' AFTER `enabled`;
ALTER TABLE `user` ADD COLUMN `external_id` VARCHAR(255) NULL DEFAULT NULL AFTER `auth_provider`;
ALTER TABLE `user` ADD UNIQUE INDEX `idx_user_external_id` (`external_id`);
