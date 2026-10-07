package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
)

// EmailCode 是一次邮箱验证码记录。
type EmailCode struct {
	ID        int64
	Email     string
	Purpose   string // register | bind | reset
	UserID    *int64
	CodeHash  string
	ExpiresAt int64
	Attempts  int
	CreatedAt int64
}

// 邮箱验证码用途。
const (
	EmailPurposeRegister = "register"
	EmailPurposeBind     = "bind"
	EmailPurposeReset    = "reset"
)

// NormalizeEmail 统一邮箱写法（小写、去空白）。
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// HashEmailCode 计算验证码摘要：以邮箱与用途为盐，避免数据库被读取后
// 直接拿明文验证码使用。
func HashEmailCode(email, purpose, code string) string {
	sum := sha256.Sum256([]byte("pagehut-email-code\x00" + email + "\x00" + purpose + "\x00" + code))
	return hex.EncodeToString(sum[:])
}

// GetUserEmail 返回用户绑定的邮箱；未绑定时返回空串。
func (s *Store) GetUserEmail(userID int64) (string, int64, error) {
	var email string
	var verifiedAt int64
	err := s.db.QueryRow(`SELECT email, verified_at FROM user_emails WHERE user_id = ?`, userID).
		Scan(&email, &verifiedAt)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return email, verifiedAt, err
}

// SetUserEmail 绑定 / 换绑邮箱；email 为空表示解绑。
func (s *Store) SetUserEmail(userID int64, email string, verifiedAt int64) error {
	if email == "" {
		_, err := s.db.Exec(`DELETE FROM user_emails WHERE user_id = ?`, userID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO user_emails (user_id, email, verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			email = excluded.email, verified_at = excluded.verified_at, updated_at = excluded.updated_at`,
		userID, email, verifiedAt, now(), now())
	return err
}

// GetUserByEmail 按邮箱查用户（OIDC 按邮箱关联本地账号时使用）。
func (s *Store) GetUserByEmail(email string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT u.id, u.username, u.password_hash, u.role, u.status,
		u.quota_projects, u.quota_project_size, u.created_at, u.updated_at
		FROM users u JOIN user_emails e ON e.user_id = u.id WHERE e.email = ?`, NormalizeEmail(email)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// EmailTaken 报告邮箱是否已被占用。
func (s *Store) EmailTaken(email string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_emails WHERE email = ?`, NormalizeEmail(email)).Scan(&n)
	return n > 0, err
}

// CreateEmailCode 写入一条验证码：同邮箱同用途只保留最新一条。
func (s *Store) CreateEmailCode(email, purpose string, userID *int64, codeHash string, expiresAt int64) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM email_codes WHERE email = ? AND purpose = ?`, email, purpose); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO email_codes (email, purpose, user_id, code_hash, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, email, purpose, userID, codeHash, expiresAt, now())
		return err
	})
}

// LatestEmailCode 返回某邮箱某用途最近一条验证码记录。
func (s *Store) LatestEmailCode(email, purpose string) (*EmailCode, error) {
	c := &EmailCode{}
	err := s.db.QueryRow(`SELECT id, email, purpose, user_id, code_hash, expires_at, attempts, created_at
		FROM email_codes WHERE email = ? AND purpose = ? ORDER BY id DESC LIMIT 1`, email, purpose).
		Scan(&c.ID, &c.Email, &c.Purpose, &c.UserID, &c.CodeHash, &c.ExpiresAt, &c.Attempts, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// IncrEmailCodeAttempts 记录一次失败尝试。
func (s *Store) IncrEmailCodeAttempts(id int64) error {
	_, err := s.db.Exec(`UPDATE email_codes SET attempts = attempts + 1 WHERE id = ?`, id)
	return err
}

// DeleteEmailCodes 清除某邮箱某用途的验证码（校验通过后调用）。
func (s *Store) DeleteEmailCodes(email, purpose string) error {
	_, err := s.db.Exec(`DELETE FROM email_codes WHERE email = ? AND purpose = ?`, email, purpose)
	return err
}

// CleanEmailCodes 清理过期验证码，返回删除条数。
func (s *Store) CleanEmailCodes() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM email_codes WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
