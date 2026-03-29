CREATE TABLE IF NOT EXISTS bug_reports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    email TEXT NOT NULL,
    description TEXT NOT NULL,
    hwid TEXT NOT NULL DEFAULT '',
    hostname TEXT NOT NULL DEFAULT '',
    os_user TEXT NOT NULL DEFAULT '',
    submitter_ip TEXT NOT NULL DEFAULT '',
    system_info TEXT NULL,
    status TEXT NOT NULL DEFAULT 'new' CHECK(status IN ('new','in_review','resolved','closed')),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_status ON bug_reports(status);
CREATE INDEX IF NOT EXISTS idx_hwid ON bug_reports(hwid);
CREATE INDEX IF NOT EXISTS idx_hostname ON bug_reports(hostname);
CREATE INDEX IF NOT EXISTS idx_os_user ON bug_reports(os_user);
CREATE INDEX IF NOT EXISTS idx_created_at ON bug_reports(created_at);

CREATE TRIGGER IF NOT EXISTS trg_bug_reports_updated_at
    AFTER UPDATE ON bug_reports
    FOR EACH ROW
    WHEN NEW.updated_at = OLD.updated_at
BEGIN
    UPDATE bug_reports SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

CREATE TABLE IF NOT EXISTS bug_report_files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    report_id INTEGER NOT NULL,
    file_role TEXT NOT NULL CHECK(file_role IN ('screenshot','mail_file','localstorage','config','system_info')),
    filename TEXT NOT NULL,
    mime_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    file_size INTEGER NOT NULL DEFAULT 0,
    data BLOB NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (report_id) REFERENCES bug_reports(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_report_id ON bug_report_files(report_id);

CREATE TABLE IF NOT EXISTS rate_limit_hwid (
    hwid TEXT PRIMARY KEY,
    window_start DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS user (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('admin','user')),
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    displayname TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS session (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    expires_at DATETIME NOT NULL,
    FOREIGN KEY (user_id) REFERENCES user(id) ON DELETE CASCADE
);
