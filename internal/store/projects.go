package store

import "database/sql"

// Project 项目状态：
// pending   待审核（不可访问）
// published 已发布（可访问）
// rejected  审核驳回（不可访问）
// suspended 已下架（不可访问）
const (
	StatusPending   = "pending"
	StatusPublished = "published"
	StatusRejected  = "rejected"
	StatusSuspended = "suspended"
)

type Project struct {
	ID           int64
	OwnerID      int64
	Slug         string
	Name         string
	Status       string
	Size         int64
	RejectReason string
	ReviewedBy   *int64
	ReviewedAt   *int64
	CreatedAt    int64
	UpdatedAt    int64
	OwnerName    string // 联表
}

const projectCols = `p.id, p.owner_id, p.slug, p.name, p.status, p.size, p.reject_reason,
	p.reviewed_by, p.reviewed_at, p.created_at, p.updated_at, u.username`

func scanProject(scanner interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	err := scanner.Scan(&p.ID, &p.OwnerID, &p.Slug, &p.Name, &p.Status, &p.Size,
		&p.RejectReason, &p.ReviewedBy, &p.ReviewedAt, &p.CreatedAt, &p.UpdatedAt, &p.OwnerName)
	if err != nil {
		return nil, err
	}
	return p, nil
}

const projectFrom = ` FROM projects p JOIN users u ON u.id = p.owner_id `

func (s *Store) CreateProject(ownerID int64, slug, name string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO projects (owner_id, slug, name, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, ownerID, slug, name, StatusPending, now(), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetProject(id int64) (*Project, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectCols+projectFrom+` WHERE p.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *Store) GetProjectBySlug(slug string) (*Project, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectCols+projectFrom+` WHERE p.slug = ?`, slug))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *Store) ListProjectsByOwner(ownerID int64) ([]*Project, error) {
	rows, err := s.db.Query(`SELECT `+projectCols+projectFrom+` WHERE p.owner_id = ? ORDER BY p.id DESC`, ownerID)
	return collectProjects(rows, err)
}

func (s *Store) ListProjectsByStatus(status string) ([]*Project, error) {
	rows, err := s.db.Query(`SELECT `+projectCols+projectFrom+` WHERE p.status = ? ORDER BY p.updated_at`, status)
	return collectProjects(rows, err)
}

func (s *Store) ListAllProjects() ([]*Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + projectFrom + ` ORDER BY p.id DESC`)
	return collectProjects(rows, err)
}

func collectProjects(rows *sql.Rows, err error) ([]*Project, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OwnerUsage 返回用户的项目数与内容总字节数。
func (s *Store) OwnerUsage(ownerID int64) (count int64, size int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM projects WHERE owner_id = ?`,
		ownerID).Scan(&count, &size)
	return
}

// UpdateProjectStatus 更新项目状态。reviewerID 非 nil 时记录审核人与时间。
func (s *Store) UpdateProjectStatus(id int64, status, reason string, reviewerID *int64) error {
	if reviewerID != nil {
		_, err := s.db.Exec(`UPDATE projects SET status = ?, reject_reason = ?,
			reviewed_by = ?, reviewed_at = ?, updated_at = ? WHERE id = ?`,
			status, reason, reviewerID, now(), now(), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE projects SET status = ?, reject_reason = '', updated_at = ? WHERE id = ?`,
		status, now(), id)
	return err
}

func (s *Store) UpdateProjectSize(id int64, size int64) error {
	_, err := s.db.Exec(`UPDATE projects SET size = ?, updated_at = ? WHERE id = ?`, size, now(), id)
	return err
}

func (s *Store) UpdateProjectMeta(id int64, name, slug string) error {
	_, err := s.db.Exec(`UPDATE projects SET name = ?, slug = ?, updated_at = ? WHERE id = ?`,
		name, slug, now(), id)
	return err
}

func (s *Store) DeleteProject(id int64) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

// ---------- 审核记录 ----------

type Review struct {
	ID           int64
	ProjectID    int64
	ReviewerID   int64
	ReviewerName string
	Action       string // approve | reject
	Reason       string
	CreatedAt    int64
}

func (s *Store) AddReview(projectID, reviewerID int64, action, reason string) error {
	_, err := s.db.Exec(`INSERT INTO reviews (project_id, reviewer_id, action, reason, created_at)
		VALUES (?, ?, ?, ?, ?)`, projectID, reviewerID, action, reason, now())
	return err
}

func (s *Store) ListReviewsByProject(projectID int64) ([]*Review, error) {
	rows, err := s.db.Query(`SELECT r.id, r.project_id, r.reviewer_id, u.username, r.action, r.reason, r.created_at
		FROM reviews r JOIN users u ON u.id = r.reviewer_id
		WHERE r.project_id = ? ORDER BY r.id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Review
	for rows.Next() {
		r := &Review{}
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ReviewerID, &r.ReviewerName, &r.Action, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
