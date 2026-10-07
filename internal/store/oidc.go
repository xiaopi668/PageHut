package store

import "database/sql"

// OIDCIdentity 是 OIDC 账号与本地用户的绑定关系。
type OIDCIdentity struct {
	ID        int64
	Issuer    string
	Subject   string
	UserID    int64
	Email     string
	CreatedAt int64
	UpdatedAt int64
}

// GetOIDCIdentity 按 (issuer, subject) 查绑定关系；不存在返回 (nil, nil)。
func (s *Store) GetOIDCIdentity(issuer, subject string) (*OIDCIdentity, error) {
	it := &OIDCIdentity{}
	err := s.db.QueryRow(`SELECT id, issuer, subject, user_id, email, created_at, updated_at
		FROM oidc_identities WHERE issuer = ? AND subject = ?`, issuer, subject).
		Scan(&it.ID, &it.Issuer, &it.Subject, &it.UserID, &it.Email, &it.CreatedAt, &it.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return it, nil
}

// LinkOIDCIdentity 建立 / 更新 OIDC 绑定。
func (s *Store) LinkOIDCIdentity(issuer, subject string, userID int64, email string) error {
	_, err := s.db.Exec(`INSERT INTO oidc_identities (issuer, subject, user_id, email, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(issuer, subject) DO UPDATE SET
			user_id = excluded.user_id, email = excluded.email, updated_at = excluded.updated_at`,
		issuer, subject, userID, email, now(), now())
	return err
}

// UnlinkOIDCIdentities 解除某用户的全部 OIDC 绑定。
func (s *Store) UnlinkOIDCIdentities(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM oidc_identities WHERE user_id = ?`, userID)
	return err
}

// ListOIDCIdentities 返回某用户的 OIDC 绑定。
func (s *Store) ListOIDCIdentities(userID int64) ([]*OIDCIdentity, error) {
	rows, err := s.db.Query(`SELECT id, issuer, subject, user_id, email, created_at, updated_at
		FROM oidc_identities WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OIDCIdentity
	for rows.Next() {
		it := &OIDCIdentity{}
		if err := rows.Scan(&it.ID, &it.Issuer, &it.Subject, &it.UserID, &it.Email, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
