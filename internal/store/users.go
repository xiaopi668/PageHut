package store

import (
	"database/sql"
)

// User 用户。角色：user 普通用户 / reviewer 审核员 / admin 管理员。
type User struct {
	ID               int64
	Username         string
	PasswordHash     string
	Role             string
	Status           string
	QuotaProjects    *int   // nil = 使用系统默认免费额度
	QuotaProjectSize *int64 // nil = 使用系统默认免费额度（字节）
	CreatedAt        int64
	UpdatedAt        int64
}

func (u *User) IsAdmin() bool    { return u.Role == "admin" }
func (u *User) IsReviewer() bool { return u.Role == "reviewer" || u.Role == "admin" }

const userCols = `id, username, password_hash, role, status, quota_projects, quota_project_size, created_at, updated_at`

func scanUser(scanner interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	err := scanner.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status,
		&u.QuotaProjects, &u.QuotaProjectSize, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) CountUsers() (int64, error) {
	var c int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&c)
	return c, err
}

func (s *Store) CountAdmins() (int64, error) {
	var c int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'`).Scan(&c)
	return c, err
}

func (s *Store) CreateUser(username, passwordHash, role string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, role, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, username, passwordHash, role, now(), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetUserByID(id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserProfile 更新角色/状态/额度覆盖（传 nil 表示恢复系统默认额度）。
func (s *Store) UpdateUserProfile(id int64, role, status string, qp *int, qps *int64) error {
	_, err := s.db.Exec(`UPDATE users SET role = ?, status = ?, quota_projects = ?,
		quota_project_size = ?, updated_at = ? WHERE id = ?`,
		role, status, qp, qps, now(), id)
	return err
}

func (s *Store) UpdateUserPassword(id int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, now(), id)
	return err
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// ---------- 会话 ----------

func (s *Store) CreateSession(token string, userID, expiresAt int64) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)`, token, userID, expiresAt, now())
	return err
}

// GetUserBySession 返回会话对应的用户；无效或已过期返回 (nil, nil)。
func (s *Store) GetUserBySession(token string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT u.id, u.username, u.password_hash, u.role, u.status,
		u.quota_projects, u.quota_project_size, u.created_at, u.updated_at
		FROM users u
		JOIN sessions s ON s.user_id = u.id
		WHERE s.token = ? AND s.expires_at > ?`, token, now()))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// DeleteOtherSessions 注销用户除 keepToken 外的所有会话（改密码用）。
func (s *Store) DeleteOtherSessions(userID int64, keepToken string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token != ?`, userID, keepToken)
	return err
}

// CleanSessions 清理过期会话，返回删除数量。
func (s *Store) CleanSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- 邀请码 ----------

type Invite struct {
	Code        string
	CreatedBy   int64
	CreatorName string
	UsedBy      *int64
	UsedByName  *string
	UsedAt      *int64
	CreatedAt   int64
}

func (s *Store) CreateInvite(code string, createdBy int64) error {
	_, err := s.db.Exec(`INSERT INTO invites (code, created_by, created_at) VALUES (?, ?, ?)`,
		code, createdBy, now())
	return err
}

func (s *Store) ListInvites() ([]*Invite, error) {
	rows, err := s.db.Query(`SELECT i.code, i.created_by, c.username, i.used_by, u.username, i.used_at, i.created_at
		FROM invites i
		JOIN users c ON c.id = i.created_by
		LEFT JOIN users u ON u.id = i.used_by
		ORDER BY i.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invite
	for rows.Next() {
		inv := &Invite{}
		if err := rows.Scan(&inv.Code, &inv.CreatedBy, &inv.CreatorName,
			&inv.UsedBy, &inv.UsedByName, &inv.UsedAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ConsumeInvite 消耗一个未使用的邀请码，返回是否成功。
func (s *Store) ConsumeInvite(code string, userID int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE invites SET used_by = ?, used_at = ? WHERE code = ? AND used_by IS NULL`,
		userID, now(), code)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) DeleteInvite(code string) error {
	_, err := s.db.Exec(`DELETE FROM invites WHERE code = ?`, code)
	return err
}
