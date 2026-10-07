// Package db 负责打开 SQLite 数据库并维护表结构。
// 使用 modernc.org/sqlite 纯 Go 驱动，无需 CGO，便于交叉编译。
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	username          TEXT    NOT NULL UNIQUE,
	password_hash     TEXT    NOT NULL,
	role              TEXT    NOT NULL DEFAULT 'user',      -- user | reviewer | admin
	status            TEXT    NOT NULL DEFAULT 'active',    -- active | disabled
	quota_projects    INTEGER,                              -- NULL = 使用系统默认
	quota_project_size INTEGER,                             -- NULL = 使用系统默认（字节）
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	slug          TEXT    NOT NULL UNIQUE,                -- 访问路径（站点域名/项目路径 的第一段），全局唯一
	name          TEXT    NOT NULL,
	status        TEXT    NOT NULL DEFAULT 'pending',     -- pending | published | rejected | suspended
	size          INTEGER NOT NULL DEFAULT 0,             -- 当前内容总大小（字节）
	reject_reason TEXT    NOT NULL DEFAULT '',
	reviewed_by   INTEGER,
	reviewed_at   INTEGER,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS reviews (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	reviewer_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	action      TEXT    NOT NULL,                       -- approve | reject
	reason      TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS domains (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	domain       TEXT    NOT NULL UNIQUE,                -- 小写、不带端口
	status       TEXT    NOT NULL DEFAULT 'pending',     -- pending | verified | active | rejected
	verify_token TEXT    NOT NULL,
	note         TEXT    NOT NULL DEFAULT '',
	reviewed_by  INTEGER,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS invites (
	code       TEXT    PRIMARY KEY,
	created_by INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	used_by    INTEGER,
	used_at    INTEGER,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notifications (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title      TEXT    NOT NULL,
	body       TEXT    NOT NULL DEFAULT '',
	read       INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	actor_id   INTEGER,
	actor_name TEXT    NOT NULL DEFAULT '',
	action     TEXT    NOT NULL,
	detail     TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS user_emails (
	user_id     INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	email       TEXT    NOT NULL UNIQUE,                -- 小写
	verified_at INTEGER NOT NULL DEFAULT 0,             -- 完成邮箱验证的时间
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS email_codes (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	email      TEXT    NOT NULL,
	purpose    TEXT    NOT NULL,                        -- register | bind | reset
	user_id    INTEGER,
	code_hash  TEXT    NOT NULL,                        -- sha256(盐 + 验证码)
	expires_at INTEGER NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS oidc_identities (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	issuer     TEXT    NOT NULL,
	subject    TEXT    NOT NULL,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	email      TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(issuer, subject)
);

CREATE INDEX IF NOT EXISTS idx_projects_owner ON projects(owner_id);
CREATE INDEX IF NOT EXISTS idx_projects_status ON projects(status);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, read);
CREATE INDEX IF NOT EXISTS idx_email_codes_lookup ON email_codes(email, purpose);
CREATE INDEX IF NOT EXISTS idx_oidc_identities_user ON oidc_identities(user_id);
`

// Open 打开（必要时创建）数据库并初始化表结构。
func Open(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	dsn := "file:" + filepath.Join(dataDir, "pagehut.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// WAL 允许「多读 + 单写」并发：给读请求留出连接，避免静态站点请求
	// 全部排队在同一个连接上。写冲突由 busy_timeout(10000) 兜底重试。
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(8)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	return d, nil
}
