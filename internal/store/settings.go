package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Settings 是运行期系统设置（全部可由管理员在后台修改）。
type Settings struct {
	SiteName         string // 站点名称，显示在面板标题
	RegistrationMode string // public | invite | closed
	ReviewEnabled    bool   // 审核总开关
	MaxProjectSize   int64  // 全站单个项目大小硬上限（字节）
	FreeProjectCount int    // 免费用户项目数量上限
	FreeProjectSize  int64  // 免费用户单个项目大小上限（字节）
	SitesHost        string // 站点域名，如 sites.example.com；项目以 <站点域名>/<项目路径>/ 访问，为空表示未启用
	PanelHost        string // 面板域名；必须与站点域名不同，为空时只有 IP / localhost 能进面板
}

const (
	keySiteName         = "site_name"
	keyRegistrationMode = "registration_mode"
	keyReviewEnabled    = "review_enabled"
	keyMaxProjectSize   = "max_project_size"
	keyFreeProjCount    = "free_project_count"
	keyFreeProjSize     = "free_project_size"
	keySitesHost        = "sites_host"
	keyPanelHost        = "panel_host"
)

func defaultSettings() *Settings {
	return &Settings{
		SiteName:         "PageHut",
		RegistrationMode: "closed",
		ReviewEnabled:    true,
		MaxProjectSize:   100 << 20, // 100MB
		FreeProjectCount: 5,
		FreeProjectSize:  50 << 20, // 50MB
	}
}

// DefaultSettings 返回一份默认设置（数据库缺项时的回退值）。
func DefaultSettings() *Settings { return defaultSettings() }

// GetSettings 读取全部设置，缺失项回退到默认值。
func (s *Store) GetSettings() (*Settings, error) {
	st := defaultSettings()
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if v, ok := m[keySiteName]; ok && v != "" {
		st.SiteName = v
	}
	if v, ok := m[keyRegistrationMode]; ok {
		switch v {
		case "public", "invite", "closed":
			st.RegistrationMode = v
		}
	}
	if v, ok := m[keyReviewEnabled]; ok {
		st.ReviewEnabled = v == "1"
	}
	if v, ok := m[keyMaxProjectSize]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			st.MaxProjectSize = n
		}
	}
	if v, ok := m[keyFreeProjCount]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			st.FreeProjectCount = n
		}
	}
	if v, ok := m[keyFreeProjSize]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			st.FreeProjectSize = n
		}
	}
	st.SitesHost = normalizeHost(m[keySitesHost])
	st.PanelHost = normalizeHost(m[keyPanelHost])
	return st, nil
}

// UpdateSettings 批量更新设置（key 必须是已知键，否则忽略）。
func (s *Store) UpdateSettings(kv map[string]string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		for k, v := range kv {
			switch k {
			case keySiteName, keyRegistrationMode, keyReviewEnabled, keyMaxProjectSize,
				keyFreeProjCount, keyFreeProjSize, keySitesHost, keyPanelHost:
			default:
				continue
			}
			_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// normalizeHost 统一主机名：小写、去端口、去末尾点。
func normalizeHost(h string) string {
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
