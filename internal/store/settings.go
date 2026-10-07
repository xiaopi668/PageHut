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

	// ---------- 人机验证 ----------
	CaptchaProvider    string // off | turnstile | image | slider
	CaptchaOnLogin     bool   // 登录是否也要过人机验证（注册与发送邮箱验证码始终需要）
	TurnstileSiteKey   string // Cloudflare Turnstile sitekey
	TurnstileSecret    string // Cloudflare Turnstile secret（后台不回显）
	TurnstileHostnames string // 额外允许的 hostname，逗号分隔；留空表示只允许当前面板 Host

	// ---------- 邮箱（SMTP）----------
	SMTPHost            string // SMTP 服务器，留空表示不启用邮件功能
	SMTPPort            int
	SMTPUser            string
	SMTPPass            string // 后台不回显
	SMTPFrom            string // 发件人地址（多数服务商要求与账号一致）
	SMTPTLS             string // starttls | ssl | none
	EmailVerifyRequired bool   // 注册必须完成邮箱验证

	// ---------- OIDC ----------
	OIDCEnabled      bool
	OIDCIssuer       string // 例如 https://accounts.example.com
	OIDCClientID     string
	OIDCClientSecret string // 后台不回显
	OIDCScopes       string // 空格分隔，默认 openid email profile
	OIDCButtonLabel  string // 登录页按钮文案
	OIDCDefaultRole  string // 自动建号时的角色：user | reviewer | admin
}

// CaptchaEnabled 报告当前是否配置了可用的人机验证。
func (s *Settings) CaptchaEnabled() bool {
	switch s.CaptchaProvider {
	case "turnstile":
		return s.TurnstileSiteKey != "" && s.TurnstileSecret != ""
	case "image", "slider":
		return true
	}
	return false
}

// EmailEnabled 报告 SMTP 是否配置齐全（服务器与发件人都填了）。
func (s *Settings) EmailEnabled() bool { return s.SMTPHost != "" && s.SMTPFrom != "" }

// OIDCReady 报告 OIDC 登录是否可用（开关打开且三项必填齐全）。
func (s *Settings) OIDCReady() bool {
	return s.OIDCEnabled && s.OIDCIssuer != "" && s.OIDCClientID != ""
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

	keyCaptchaProvider  = "captcha_provider"
	keyCaptchaOnLogin   = "captcha_on_login"
	keyTurnstileSiteKey = "turnstile_site_key"
	keyTurnstileSecret  = "turnstile_secret"
	keyTurnstileHosts   = "turnstile_hostnames"

	keySMTPHost       = "smtp_host"
	keySMTPPort       = "smtp_port"
	keySMTPUser       = "smtp_user"
	keySMTPPass       = "smtp_pass"
	keySMTPFrom       = "smtp_from"
	keySMTPTLS        = "smtp_tls"
	keyEmailVerifyReq = "email_verify_required"

	keyOIDCEnabled      = "oidc_enabled"
	keyOIDCIssuer       = "oidc_issuer"
	keyOIDCClientID     = "oidc_client_id"
	keyOIDCClientSecret = "oidc_client_secret"
	keyOIDCScopes       = "oidc_scopes"
	keyOIDCButtonLabel  = "oidc_button_label"
	keyOIDCDefaultRole  = "oidc_default_role"
)

func defaultSettings() *Settings {
	return &Settings{
		SiteName:         "PageHut",
		RegistrationMode: "closed",
		ReviewEnabled:    true,
		MaxProjectSize:   100 << 20, // 100MB
		FreeProjectCount: 5,
		FreeProjectSize:  50 << 20, // 50MB

		CaptchaProvider: "off",
		SMTPPort:        587,
		SMTPTLS:         "starttls",
		OIDCScopes:      "openid email profile",
		OIDCButtonLabel: "使用 OIDC 登录",
		OIDCDefaultRole: "user",
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

	// 人机验证
	st.CaptchaProvider = pick(m[keyCaptchaProvider], "off", "off", "turnstile", "image", "slider")
	st.CaptchaOnLogin = m[keyCaptchaOnLogin] == "1"
	st.TurnstileSiteKey = strings.TrimSpace(m[keyTurnstileSiteKey])
	st.TurnstileSecret = strings.TrimSpace(m[keyTurnstileSecret])
	st.TurnstileHostnames = strings.TrimSpace(m[keyTurnstileHosts])

	// 邮箱
	st.SMTPHost = strings.TrimSpace(m[keySMTPHost])
	st.SMTPPort = intVal(m[keySMTPPort], 587)
	st.SMTPUser = strings.TrimSpace(m[keySMTPUser])
	st.SMTPPass = m[keySMTPPass]
	st.SMTPFrom = strings.TrimSpace(m[keySMTPFrom])
	st.SMTPTLS = pick(m[keySMTPTLS], "starttls", "starttls", "ssl", "none")
	st.EmailVerifyRequired = m[keyEmailVerifyReq] == "1"

	// OIDC
	st.OIDCEnabled = m[keyOIDCEnabled] == "1"
	st.OIDCIssuer = strings.TrimSpace(m[keyOIDCIssuer])
	st.OIDCClientID = strings.TrimSpace(m[keyOIDCClientID])
	st.OIDCClientSecret = m[keyOIDCClientSecret]
	if v := strings.TrimSpace(m[keyOIDCScopes]); v != "" {
		st.OIDCScopes = v
	}
	if v := strings.TrimSpace(m[keyOIDCButtonLabel]); v != "" {
		st.OIDCButtonLabel = v
	}
	st.OIDCDefaultRole = pick(m[keyOIDCDefaultRole], "user", "user", "reviewer", "admin")
	return st, nil
}

// UpdateSettings 批量更新设置（key 必须是已知键，否则忽略）。
func (s *Store) UpdateSettings(kv map[string]string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		for k, v := range kv {
			switch k {
			case keySiteName, keyRegistrationMode, keyReviewEnabled, keyMaxProjectSize,
				keyFreeProjCount, keyFreeProjSize, keySitesHost, keyPanelHost,
				keyCaptchaProvider, keyCaptchaOnLogin, keyTurnstileSiteKey, keyTurnstileSecret,
				keyTurnstileHosts,
				keySMTPHost, keySMTPPort, keySMTPUser, keySMTPPass, keySMTPFrom, keySMTPTLS,
				keyEmailVerifyReq,
				keyOIDCEnabled, keyOIDCIssuer, keyOIDCClientID, keyOIDCClientSecret, keyOIDCScopes,
				keyOIDCButtonLabel, keyOIDCDefaultRole:
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

// pick 返回 m[key]，仅当它属于 allowed 之一，否则返回 def。
func pick(v, def string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

func intVal(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < 65536 {
		return n
	}
	return def
}

// normalizeHost 统一主机名：小写、去端口、去末尾点。
func normalizeHost(h string) string {
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
