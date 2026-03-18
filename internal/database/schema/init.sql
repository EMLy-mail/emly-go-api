CREATE TABLE IF NOT EXISTS `bug_reports` (
                                             `id` INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
                                             `name` VARCHAR(255) NOT NULL,
                                             `email` VARCHAR(255) NOT NULL,
                                             `description` TEXT NOT NULL,
                                             `hwid` VARCHAR(255) NOT NULL DEFAULT '',
                                             `hostname` VARCHAR(255) NOT NULL DEFAULT '',
                                             `os_user` VARCHAR(255) NOT NULL DEFAULT '',
                                             `submitter_ip` VARCHAR(45) NOT NULL DEFAULT '',
                                             `system_info` JSON NULL,
                                             `status` ENUM('new', 'in_review', 'resolved', 'closed') NOT NULL DEFAULT 'new',
                                             `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
                                             `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
                                             INDEX `idx_status` (`status`),
                                             INDEX `idx_hwid` (`hwid`),
                                             INDEX `idx_hostname` (`hostname`),
                                             INDEX `idx_os_user` (`os_user`),
                                             INDEX `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `bug_report_files` (
                                                  `id` INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
                                                  `report_id` INT UNSIGNED NOT NULL,
                                                  `file_role` ENUM('screenshot', 'mail_file', 'localstorage', 'config', 'system_info') NOT NULL,
                                                  `filename` VARCHAR(255) NOT NULL,
                                                  `mime_type` VARCHAR(127) NOT NULL DEFAULT 'application/octet-stream',
                                                  `file_size` INT UNSIGNED NOT NULL DEFAULT 0,
                                                  `data` LONGBLOB NOT NULL,
                                                  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
                                                  CONSTRAINT `fk_report` FOREIGN KEY (`report_id`) REFERENCES `bug_reports`(`id`) ON DELETE CASCADE,
                                                  INDEX `idx_report_id` (`report_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `rate_limit_hwid` (
                                                 `hwid` VARCHAR(255) PRIMARY KEY,
                                                 `window_start` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
                                                 `count` INT UNSIGNED NOT NULL DEFAULT 0
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `user` (
                                      `id` VARCHAR(255) PRIMARY KEY,
                                      `username` VARCHAR(255) NOT NULL UNIQUE,
                                      `password_hash` VARCHAR(255) NOT NULL,
                                      `role` ENUM('admin', 'user') NOT NULL DEFAULT 'user',
                                      `enabled` BOOLEAN NOT NULL DEFAULT TRUE,
                                      `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
                                      `displayname` VARCHAR(255) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS `session` (
                                         `id` VARCHAR(255) PRIMARY KEY,
                                         `user_id` VARCHAR(255) NOT NULL,
                                         `expires_at` DATETIME NOT NULL,
                                         CONSTRAINT `fk_session_user` FOREIGN KEY (`user_id`) REFERENCES `user`(`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
