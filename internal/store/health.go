package store

// Ping 检查数据库是否可读（供 /healthz 健康检查使用）。
func (s *Store) Ping() error {
	var one int
	return s.db.QueryRow(`SELECT 1`).Scan(&one)
}
