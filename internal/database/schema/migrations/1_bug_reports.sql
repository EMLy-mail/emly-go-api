ALTER TABLE bug_reports ADD COLUMN hostname VARCHAR(255) NOT NULL DEFAULT '' AFTER hwid;
ALTER TABLE bug_reports ADD COLUMN os_user VARCHAR(255) NOT NULL DEFAULT '' AFTER hostname;
ALTER TABLE bug_reports ADD INDEX idx_hostname (hostname);
ALTER TABLE bug_reports ADD INDEX idx_os_user (os_user);
