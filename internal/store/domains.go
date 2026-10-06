package store

import "database/sql"

// Domain 自定义域名绑定申请。
// 状态流转：pending（待验证/待审核）→ verified（所有权已验证，等待管理员开通）→ active（生效）
// 任意阶段可被管理员 rejected，或被用户删除。
type Domain struct {
	ID          int64
	ProjectID   int64
	Domain      string
	Status      string
	VerifyToken string
	Note        string
	ReviewedBy  *int64
	CreatedAt   int64
	UpdatedAt   int64
	// 联表
	ProjectSlug string
	ProjectName string
	OwnerName   string
	OwnerID     int64
}

const domainCols = `d.id, d.project_id, d.domain, d.status, d.verify_token, d.note,
	d.reviewed_by, d.created_at, d.updated_at, p.slug, p.name, u.username, u.id`

func scanDomain(scanner interface{ Scan(...any) error }) (*Domain, error) {
	d := &Domain{}
	err := scanner.Scan(&d.ID, &d.ProjectID, &d.Domain, &d.Status, &d.VerifyToken, &d.Note,
		&d.ReviewedBy, &d.CreatedAt, &d.UpdatedAt, &d.ProjectSlug, &d.ProjectName, &d.OwnerName, &d.OwnerID)
	if err != nil {
		return nil, err
	}
	return d, nil
}

const domainFrom = ` FROM domains d JOIN projects p ON p.id = d.project_id JOIN users u ON u.id = p.owner_id `

func (s *Store) CreateDomain(projectID int64, domain, token string) error {
	_, err := s.db.Exec(`INSERT INTO domains (project_id, domain, verify_token, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, projectID, domain, token, now(), now())
	return err
}

func (s *Store) GetDomain(id int64) (*Domain, error) {
	d, err := scanDomain(s.db.QueryRow(`SELECT `+domainCols+domainFrom+` WHERE d.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

// GetDomainByHost 按域名精确匹配（小写、不带端口）。
func (s *Store) GetDomainByHost(host string) (*Domain, error) {
	d, err := scanDomain(s.db.QueryRow(`SELECT `+domainCols+domainFrom+` WHERE d.domain = ?`, host))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) ListDomainsByProject(projectID int64) ([]*Domain, error) {
	rows, err := s.db.Query(`SELECT `+domainCols+domainFrom+` WHERE d.project_id = ? ORDER BY d.id DESC`, projectID)
	return collectDomains(rows, err)
}

func (s *Store) ListDomains(status string) ([]*Domain, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.Query(`SELECT ` + domainCols + domainFrom + ` ORDER BY d.id DESC`)
	} else {
		rows, err = s.db.Query(`SELECT `+domainCols+domainFrom+` WHERE d.status = ? ORDER BY d.id DESC`, status)
	}
	return collectDomains(rows, err)
}

func collectDomains(rows *sql.Rows, err error) ([]*Domain, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDomainStatus 更新域名状态。reviewerID 非 nil 时记录审核人。
func (s *Store) UpdateDomainStatus(id int64, status string, reviewerID *int64) error {
	if reviewerID != nil {
		_, err := s.db.Exec(`UPDATE domains SET status = ?, reviewed_by = ?, updated_at = ? WHERE id = ?`,
			status, reviewerID, now(), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE domains SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

func (s *Store) DeleteDomain(id int64) error {
	_, err := s.db.Exec(`DELETE FROM domains WHERE id = ?`, id)
	return err
}
