package web

import (
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"pagehut/internal/store"
)

// ---------- 系统设置 ----------

func (w *Web) adminSettingsPage(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.auth(rw, r, "admin"); !ok {
		return
	}
	st, err := w.st.GetSettings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	w.render(rw, r, http.StatusOK, "settings", "系统设置", map[string]any{
		"S": st,
		// 便于管理员直接复制到 IdP 的「允许回调地址」
		"RedirectURL": w.oidcRedirectURL(r),
	})
}

func (w *Web) adminSettingsSubmit(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	st, _ := w.st.GetSettings()

	siteName := strings.TrimSpace(r.FormValue("site_name"))
	mode := r.FormValue("registration_mode")
	switch mode {
	case "public", "invite", "closed":
	default:
		mode = st.RegistrationMode
	}
	review := "0"
	if r.FormValue("review_enabled") == "1" {
		review = "1"
	}
	maxMB, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("max_project_size_mb")), 10, 64)
	if err != nil || maxMB <= 0 {
		w.errorPage(rw, r, http.StatusBadRequest, "单个项目大小上限必须为正整数（MB）。")
		return
	}
	freeCount, err := strconv.Atoi(strings.TrimSpace(r.FormValue("free_project_count")))
	if err != nil || freeCount < 0 {
		w.errorPage(rw, r, http.StatusBadRequest, "免费项目数量必须为非负整数。")
		return
	}
	freeMB, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("free_project_size_mb")), 10, 64)
	if err != nil || freeMB < 0 {
		w.errorPage(rw, r, http.StatusBadRequest, "免费单项目大小必须为非负整数（MB）。")
		return
	}
	if freeMB > maxMB {
		freeMB = maxMB
	}
	sitesHost := parseHostInput(r.FormValue("sites_host"))
	panelHost := parseHostInput(r.FormValue("panel_host"))
	if sitesHost != "" && !validDomain(sitesHost) {
		w.errorPage(rw, r, http.StatusBadRequest, "站点域名格式不正确。")
		return
	}
	if panelHost != "" && !validDomain(panelHost) {
		w.errorPage(rw, r, http.StatusBadRequest, "面板域名格式不正确。")
		return
	}
	// 站点域名与面板域名必须分开：用户站点里跑的是上传的任意 JS，
	// 一旦同源就能读取面板数据、以登录者身份调用面板接口。
	if sitesHost != "" && sitesHost == panelHost {
		w.errorPage(rw, r, http.StatusBadRequest, "站点域名不能与面板域名相同：否则用户站点会与面板同源，存在安全风险。")
		return
	}
	if siteName == "" {
		siteName = "PageHut"
	}

	// ---------- 人机验证 ----------
	captchaProvider := r.FormValue("captcha_provider")
	switch captchaProvider {
	case captchaOff, captchaTurnstile, captchaImage, captchaSlider:
	default:
		captchaProvider = st.CaptchaProvider
	}
	captchaOnLogin := "0"
	if r.FormValue("captcha_on_login") == "1" {
		captchaOnLogin = "1"
	}
	turnstileSiteKey := strings.TrimSpace(r.FormValue("turnstile_site_key"))
	// 密钥留空表示「不修改」，避免把已保存的密钥回显到表单里
	turnstileSecret := keepSecret(r.FormValue("turnstile_secret"), st.TurnstileSecret)
	turnstileHosts := strings.TrimSpace(r.FormValue("turnstile_hostnames"))
	if captchaProvider == captchaTurnstile && (turnstileSiteKey == "" || turnstileSecret == "") {
		w.errorPage(rw, r, http.StatusBadRequest, "选择 Cloudflare Turnstile 时必须填写 sitekey 与 secret。")
		return
	}

	// ---------- 邮件（SMTP）----------
	smtpHost := strings.TrimSpace(r.FormValue("smtp_host"))
	// 端口缺省时沿用当前值；只有显式填了非法值才报错
	smtpPort := st.SMTPPort
	if v := strings.TrimSpace(r.FormValue("smtp_port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			w.errorPage(rw, r, http.StatusBadRequest, "SMTP 端口必须是 1-65535 之间的整数。")
			return
		}
		smtpPort = n
	}
	smtpUser := strings.TrimSpace(r.FormValue("smtp_user"))
	smtpPass := keepSecret(r.FormValue("smtp_pass"), st.SMTPPass)
	smtpFrom := strings.TrimSpace(r.FormValue("smtp_from"))
	smtpTLS := r.FormValue("smtp_tls")
	switch smtpTLS {
	case "starttls", "ssl", "none":
	default:
		smtpTLS = st.SMTPTLS
	}
	if smtpHost != "" && !validEmail(smtpFrom) {
		w.errorPage(rw, r, http.StatusBadRequest, "配置了 SMTP 服务器时，发件人必须是合法邮箱地址。")
		return
	}
	emailVerify := "0"
	if r.FormValue("email_verify_required") == "1" {
		emailVerify = "1"
	}
	if emailVerify == "1" && (smtpHost == "" || smtpFrom == "") {
		w.errorPage(rw, r, http.StatusBadRequest, "开启「注册必须验证邮箱」前，请先填写 SMTP 服务器与发件人。")
		return
	}

	// ---------- OIDC ----------
	oidcEnabled := "0"
	if r.FormValue("oidc_enabled") == "1" {
		oidcEnabled = "1"
	}
	oidcIssuer := strings.TrimRight(strings.TrimSpace(r.FormValue("oidc_issuer")), "/")
	oidcClientID := strings.TrimSpace(r.FormValue("oidc_client_id"))
	oidcSecret := keepSecret(r.FormValue("oidc_client_secret"), st.OIDCClientSecret)
	oidcScopes := strings.TrimSpace(r.FormValue("oidc_scopes"))
	oidcLabel := strings.TrimSpace(r.FormValue("oidc_button_label"))
	oidcRole := r.FormValue("oidc_default_role")
	switch oidcRole {
	case "user", "reviewer", "admin":
	default:
		oidcRole = st.OIDCDefaultRole
	}
	if oidcIssuer != "" && !validIssuerURL(oidcIssuer) {
		w.errorPage(rw, r, http.StatusBadRequest, "OIDC Issuer 必须是 http/https 开头的完整地址（例如 https://accounts.example.com）。")
		return
	}
	if oidcEnabled == "1" && (oidcIssuer == "" || oidcClientID == "") {
		w.errorPage(rw, r, http.StatusBadRequest, "启用 OIDC 前必须填写 Issuer 与 Client ID。")
		return
	}

	kv := map[string]string{
		"site_name":          siteName,
		"registration_mode":  mode,
		"review_enabled":     review,
		"max_project_size":   strconv.FormatInt(maxMB<<20, 10),
		"free_project_count": strconv.Itoa(freeCount),
		"free_project_size":  strconv.FormatInt(freeMB<<20, 10),
		"sites_host":         sitesHost,
		"panel_host":         panelHost,

		"captcha_provider":      captchaProvider,
		"captcha_on_login":      captchaOnLogin,
		"turnstile_site_key":    turnstileSiteKey,
		"turnstile_secret":      turnstileSecret,
		"turnstile_hostnames":   turnstileHosts,
		"smtp_host":             smtpHost,
		"smtp_port":             strconv.Itoa(smtpPort),
		"smtp_user":             smtpUser,
		"smtp_pass":             smtpPass,
		"smtp_from":             smtpFrom,
		"smtp_tls":              smtpTLS,
		"email_verify_required": emailVerify,
		"oidc_enabled":          oidcEnabled,
		"oidc_issuer":           oidcIssuer,
		"oidc_client_id":        oidcClientID,
		"oidc_client_secret":    oidcSecret,
		"oidc_scopes":           oidcScopes,
		"oidc_button_label":     oidcLabel,
		"oidc_default_role":     oidcRole,
	}
	if err := w.st.UpdateSettings(kv); err != nil {
		log.Printf("[web] 保存设置失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "保存失败")
		return
	}
	w.invalidateSettings() // 让 ServeHTTP / 渲染路径立即用上新设置
	w.st.Audit(&u.ID, u.Username, "settings.update",
		"mode="+mode+" review="+review+" maxMB="+strconv.FormatInt(maxMB, 10)+
			" freeCount="+strconv.Itoa(freeCount)+" freeMB="+strconv.FormatInt(freeMB, 10)+
			" sites="+sitesHost+" panel="+panelHost+
			" captcha="+captchaProvider+" emailVerify="+emailVerify+" oidc="+oidcEnabled)
	flash(rw, r, "设置已保存。")
	http.Redirect(rw, r, "/admin/settings", http.StatusSeeOther)
}

// keepSecret 处理密钥类字段：表单留空表示沿用已保存的值（不回显、不误清空）。
func keepSecret(submitted, current string) string {
	if s := strings.TrimSpace(submitted); s != "" {
		return s
	}
	return current
}

// validIssuerURL 校验 OIDC Issuer 是否是合法的 http/https 绝对地址。
func validIssuerURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// parseHostInput 清理用户输入的域名（去协议、去端口、小写）。
func parseHostInput(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.ToLower(hostOnly(s))
}

// ---------- 用户管理 ----------

func (w *Web) adminUsersPage(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.auth(rw, r, "admin"); !ok {
		return
	}
	users, err := w.st.ListUsers()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	w.render(rw, r, http.StatusOK, "users", "用户管理", map[string]any{
		"Users": users,
	})
}

func (w *Web) adminUserNewPage(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.auth(rw, r, "admin"); !ok {
		return
	}
	w.render(rw, r, http.StatusOK, "user_new", "创建用户", map[string]any{
		"Error": "", "Username": "",
	})
}

func (w *Web) adminUserNewSubmit(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")

	fail := func(msg string) {
		w.render(rw, r, http.StatusBadRequest, "user_new", "创建用户", map[string]any{
			"Error": msg, "Username": username,
		})
	}
	if !validUsername(username) {
		fail("用户名需为 2-32 位字母、数字、下划线或短横线。")
		return
	}
	if len(password) < 8 {
		fail("密码至少 8 位。")
		return
	}
	if len(password) > maxPasswordLen {
		fail("密码过长：bcrypt 上限为 72 字节（中文约 24 个字）。")
		return
	}
	if role != "user" && role != "reviewer" && role != "admin" {
		role = "user"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if _, err := w.st.CreateUser(username, string(hash), role); err != nil {
		if store.IsUniqueErr(err) {
			fail("用户名已存在。")
			return
		}
		log.Printf("[web] 创建用户失败: %v", err)
		fail("服务器错误。")
		return
	}
	w.st.Audit(&u.ID, u.Username, "user.create", username+" ("+role+")")
	flash(rw, r, "用户已创建。")
	http.Redirect(rw, r, "/admin/users", http.StatusSeeOther)
}

func (w *Web) adminUserEditPage(rw http.ResponseWriter, r *http.Request) {
	me, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	target, err := w.st.GetUserByID(id)
	if err != nil || target == nil {
		w.errorPage(rw, r, http.StatusNotFound, "用户不存在")
		return
	}
	count, size, _ := w.st.OwnerUsage(id)
	projects, _ := w.st.ListProjectsByOwner(id)
	w.render(rw, r, http.StatusOK, "user_edit", "编辑用户", map[string]any{
		"Target":   target,
		"IsSelf":   target.ID == me.ID,
		"Usage":    map[string]any{"Count": count, "Size": size},
		"Projects": projects,
	})
}

func (w *Web) adminUserSave(rw http.ResponseWriter, r *http.Request) {
	me, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	target, err := w.st.GetUserByID(id)
	if err != nil || target == nil {
		w.errorPage(rw, r, http.StatusNotFound, "用户不存在")
		return
	}

	role := r.FormValue("role")
	if role != "user" && role != "reviewer" && role != "admin" {
		role = target.Role
	}
	status := r.FormValue("status")
	if status != "active" && status != "disabled" {
		status = target.Status
	}

	// 保护：不能改自己的角色/状态；至少保留一名可用管理员
	if target.ID == me.ID && (role != target.Role || status != target.Status) {
		w.errorPage(rw, r, http.StatusBadRequest, "不能修改自己的角色或状态。")
		return
	}
	if target.Role == "admin" && (role != "admin" || status != "active") {
		admins, _ := w.st.CountAdmins()
		if admins <= 1 {
			w.errorPage(rw, r, http.StatusBadRequest, "至少需要保留一名可用管理员。")
			return
		}
	}

	// 额度覆盖：留空表示使用系统默认
	var qp *int
	if v := strings.TrimSpace(r.FormValue("quota_projects")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			qp = &n
		}
	}
	var qps *int64
	if v := strings.TrimSpace(r.FormValue("quota_project_size_mb")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			st := w.mustSettings()
			if n<<20 > st.MaxProjectSize {
				n = st.MaxProjectSize >> 20
			}
			qps = &n
			nn := *qps << 20
			qps = &nn
		}
	}

	if err := w.st.UpdateUserProfile(target.ID, role, status, qp, qps); err != nil {
		log.Printf("[web] 更新用户失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "更新失败")
		return
	}
	if pw := r.FormValue("password"); pw != "" {
		if len(pw) < 8 {
			w.errorPage(rw, r, http.StatusBadRequest, "新密码至少 8 位（其他修改已保存）。")
			return
		}
		if len(pw) > maxPasswordLen {
			w.errorPage(rw, r, http.StatusBadRequest, "新密码过长：bcrypt 上限为 72 字节（其他修改已保存）。")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
			return
		}
		w.st.UpdateUserPassword(target.ID, string(hash))
	}
	if status == "disabled" {
		w.st.DeleteUserSessions(target.ID)
	}
	w.st.Audit(&me.ID, me.Username, "user.update",
		target.Username+" role="+role+" status="+status)
	flash(rw, r, "用户信息已更新。")
	http.Redirect(rw, r, "/admin/users/"+strconv.FormatInt(target.ID, 10), http.StatusSeeOther)
}

func (w *Web) adminUserDelete(rw http.ResponseWriter, r *http.Request) {
	me, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	if id == me.ID {
		w.errorPage(rw, r, http.StatusBadRequest, "不能删除自己的账号。")
		return
	}
	target, err := w.st.GetUserByID(id)
	if err != nil || target == nil {
		w.errorPage(rw, r, http.StatusNotFound, "用户不存在")
		return
	}
	if target.Role == "admin" {
		admins, _ := w.st.CountAdmins()
		if admins <= 1 {
			w.errorPage(rw, r, http.StatusBadRequest, "至少需要保留一名可用管理员。")
			return
		}
	}
	// 先清理磁盘上的项目内容，数据库行由外键级联删除
	projects, _ := w.st.ListProjectsByOwner(id)
	for _, p := range projects {
		w.disk.Destroy(p.ID)
		w.st.DeleteProject(p.ID)
	}
	if err := w.st.DeleteUser(id); err != nil {
		log.Printf("[web] 删除用户失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "删除失败")
		return
	}
	w.st.Audit(&me.ID, me.Username, "user.delete", target.Username)
	flash(rw, r, "用户及其项目已删除。")
	http.Redirect(rw, r, "/admin/users", http.StatusSeeOther)
}

// ---------- 邀请码 ----------

func (w *Web) adminInvitesPage(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.auth(rw, r, "admin"); !ok {
		return
	}
	invites, err := w.st.ListInvites()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	st := w.mustSettings()
	w.render(rw, r, http.StatusOK, "invites", "邀请码", map[string]any{
		"Invites": invites,
		"Mode":    st.RegistrationMode,
	})
}

func (w *Web) adminInviteCreate(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	code := randCode(8)
	if err := w.st.CreateInvite(code, u.ID); err != nil {
		log.Printf("[web] 生成邀请码失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	w.st.Audit(&u.ID, u.Username, "invite.create", code)
	flash(rw, r, "已生成邀请码："+code)
	http.Redirect(rw, r, "/admin/invites", http.StatusSeeOther)
}

func (w *Web) adminInviteDelete(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	if code != "" {
		w.st.DeleteInvite(code)
		w.st.Audit(&u.ID, u.Username, "invite.delete", code)
	}
	http.Redirect(rw, r, "/admin/invites", http.StatusSeeOther)
}

// ---------- 域名审核 ----------

func (w *Web) adminDomainsPage(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.auth(rw, r, "admin"); !ok {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "pending", "verified", "active", "rejected":
	default:
		status = ""
	}
	domains, err := w.st.ListDomains(status)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	w.render(rw, r, http.StatusOK, "admin_domains", "域名管理", map[string]any{
		"Domains": domains,
		"Filter":  status,
	})
}

func (w *Web) adminDomainAction(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r, "admin")
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	d, err := w.st.GetDomain(id)
	if err != nil || d == nil {
		w.errorPage(rw, r, http.StatusNotFound, "域名申请不存在")
		return
	}
	action := r.FormValue("action")
	switch action {
	case "activate":
		if d.Status != "verified" {
			flash(rw, r, "该域名尚未完成所有权验证，无法开通。")
			break
		}
		w.st.UpdateDomainStatus(d.ID, "active", &u.ID)
		w.st.Notify(d.OwnerID, "域名已开通", "域名 "+d.Domain+" 已绑定到项目 "+d.ProjectSlug+"，解析生效后即可访问。")
		w.st.Audit(&u.ID, u.Username, "domain.activate", d.Domain)
		flash(rw, r, "域名已开通。")
	case "reject":
		w.st.UpdateDomainStatus(d.ID, "rejected", &u.ID)
		w.st.Notify(d.OwnerID, "域名申请被拒绝", "域名 "+d.Domain+" 的绑定申请已被管理员拒绝。")
		w.st.Audit(&u.ID, u.Username, "domain.reject", d.Domain)
		flash(rw, r, "已拒绝该域名申请。")
	default:
		flash(rw, r, "未知操作。")
	}
	http.Redirect(rw, r, "/admin/domains", http.StatusSeeOther)
}

// ---------- 审计日志 ----------

func (w *Web) adminAuditPage(rw http.ResponseWriter, r *http.Request) {
	if _, ok := w.auth(rw, r, "admin"); !ok {
		return
	}
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	const limit = 50
	entries, total, err := w.st.ListAudit(limit, (page-1)*limit)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	pages := int(total+limit-1) / limit
	w.render(rw, r, http.StatusOK, "audit", "操作日志", map[string]any{
		"Entries": entries,
		"Total":   total,
		"Page":    page,
		"Pages":   pages,
	})
}
