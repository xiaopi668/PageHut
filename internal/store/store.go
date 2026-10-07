// Package store 封装所有数据库访问。
package store

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
)

// Store 持有数据库连接池，所有方法并发安全。
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

// ErrUnique 用于识别唯一约束冲突（用户名/访问路径/域名重复）。
const ErrUnique = "UNIQUE constraint failed"

func IsUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrUnique)
}

func now() int64 { return time.Now().Unix() }

// Audit 写入一条操作日志，失败只记录不中断业务。
func (s *Store) Audit(actorID *int64, actorName, action, detail string) {
	_, err := s.db.Exec(`INSERT INTO audit_log (actor_id, actor_name, action, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`, actorID, actorName, action, detail, now())
	if err != nil {
		log.Printf("[store] 写入审计日志失败: %v", err)
	}
}

// AuditEntry 审计日志条目。
type AuditEntry struct {
	ID        int64
	ActorID   *int64
	ActorName string
	Action    string
	Detail    string
	CreatedAt int64
}

// ListAudit 分页读取审计日志，按时间倒序。
func (s *Store) ListAudit(limit, offset int) ([]*AuditEntry, int64, error) {
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id, actor_id, actor_name, action, detail, created_at
		FROM audit_log ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(&e.ID, &e.ActorID, &e.ActorName, &e.Action, &e.Detail, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Notification 站内通知。
type Notification struct {
	ID        int64
	UserID    int64
	Title     string
	Body      string
	Read      bool
	CreatedAt int64
}

func (s *Store) Notify(userID int64, title, body string) {
	_, err := s.db.Exec(`INSERT INTO notifications (user_id, title, body, created_at)
		VALUES (?, ?, ?, ?)`, userID, title, body, now())
	if err != nil {
		log.Printf("[store] 写入通知失败: %v", err)
	}
}

func (s *Store) ListNotifications(userID int64) ([]*Notification, error) {
	rows, err := s.db.Query(`SELECT id, user_id, title, body, read, created_at
		FROM notifications WHERE user_id = ? ORDER BY id DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Notification
	for rows.Next() {
		n := &Notification{}
		var read int
		if err := rows.Scan(&n.ID, &n.UserID, &n.Title, &n.Body, &read, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Read = read == 1
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) UnreadCount(userID int64) (int64, error) {
	var c int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read = 0`, userID).Scan(&c)
	return c, err
}

func (s *Store) MarkAllNotificationsRead(userID int64) error {
	_, err := s.db.Exec(`UPDATE notifications SET read = 1 WHERE user_id = ?`, userID)
	return err
}

// withTx 在事务中执行 fn，出错自动回滚。
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
