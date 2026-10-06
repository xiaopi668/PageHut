package web

import (
	"log"
	"net/http"
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
		w.errorPage(rw, r, http.StatusBadRequest, "子域名后缀格式不正确。")
		return
	}
	if panelHost != "" && !validDomain(panelHost) {
		w.errorPage(rw, r, http.StatusBadRequest, "面板域名格式不正确。")
		return
	}
	if siteName == "" {
		siteName = "PageHut"
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
	}
	if err := w.st.UpdateSettings(kv); err != nil {
		log.Printf("[web] 保存设置失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "保存失败")
		return
	}
	w.st.Audit(&u.ID, u.Username, "settings.update",
		"mode="+mode+" review="+review+" maxMB="+strconv.FormatInt(maxMB, 10)+
			" freeCount="+strconv.Itoa(freeCount)+" freeMB="+strconv.FormatInt(freeMB, 10)+
			" sites="+sitesHost+" panel="+panelHost)
	flash(rw, r, "设置已保存。")
	http.Redirect(rw, r, "/admin/settings", http.StatusSeeOther)
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
