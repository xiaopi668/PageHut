package web

import (
	"log"
	"math"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"pagehut/internal/storage"
	"pagehut/internal/store"
)

// quotaFor 计算用户的项目数量与单项目大小限额。
// 管理员不受数量限制；所有用户（含管理员）都受全站单项目硬上限约束。
func (w *Web) quotaFor(u *store.User, st *store.Settings) (int, int64) {
	if u.IsAdmin() {
		return math.MaxInt32, st.MaxProjectSize
	}
	cnt := st.FreeProjectCount
	if u.QuotaProjects != nil {
		cnt = *u.QuotaProjects
	}
	size := st.FreeProjectSize
	if u.QuotaProjectSize != nil {
		size = *u.QuotaProjectSize
	}
	if size > st.MaxProjectSize {
		size = st.MaxProjectSize
	}
	return cnt, size
}

// updateProjectSize 重新统计项目内容大小并写回数据库。
func (w *Web) updateProjectSize(p *store.Project) {
	files, size, err := w.disk.Usage(p.ID)
	if err != nil {
		log.Printf("[web] 统计项目 %d 大小失败: %v", p.ID, err)
		return
	}
	if err := w.st.UpdateProjectSize(p.ID, size); err != nil {
		log.Printf("[web] 更新项目 %d 大小失败: %v", p.ID, err)
	}
	_ = files
}

// ensureMutable 拒绝对「已下架」项目的任何内容变更。
// 下架是管理员的处置动作，不能被所有者用一次上传 / 编辑就覆盖掉；
// 要恢复必须先由管理员在审核页执行「发布」。
func (w *Web) ensureMutable(rw http.ResponseWriter, r *http.Request, p *store.Project) bool {
	if p.Status == store.StatusSuspended {
		w.errorPage(rw, r, http.StatusForbidden,
			"该项目已被管理员下架，无法修改内容；如需恢复请联系管理员。")
		return false
	}
	return true
}

// contentChanged 内容变更后统一处理：更新大小，并在审核开启时把已发布项目退回待审核。
func (w *Web) contentChanged(p *store.Project, st *store.Settings) {
	w.updateProjectSize(p)
	if st.ReviewEnabled && p.Status == store.StatusPublished {
		if err := w.st.UpdateProjectStatus(p.ID, store.StatusPending, "", nil); err == nil {
			w.notifyReviewers("内容更新待审核", "项目「"+p.Name+"」("+p.Slug+") 内容有更新，需要重新审核。")
		}
	}
}

var slugCleaner = regexp.MustCompile(`[^a-z0-9-]+`)

// normalizeSlug 把用户输入整理成合法子域名前缀。
func normalizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = slugCleaner.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// ---------- 面板首页 ----------

func (w *Web) dashboardPage(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	st, err := w.st.GetSettings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	list, err := w.st.ListProjectsByOwner(u.ID)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	count, size, err := w.st.OwnerUsage(u.ID)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	cntLimit, sizeLimit := w.quotaFor(u, st)
	w.render(rw, r, http.StatusOK, "dashboard", "我的项目", map[string]any{
		"Projects":   list,
		"Count":      count,
		"Size":       size,
		"CountLimit": cntLimit,
		"SizeLimit":  sizeLimit,
		"ReviewOn":   st.ReviewEnabled,
	})
}

// ---------- 项目创建 ----------

func (w *Web) projectNewPage(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	st, err := w.st.GetSettings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	cnt, size := w.quotaFor(u, st)
	used, _, _ := w.st.OwnerUsage(u.ID)
	w.render(rw, r, http.StatusOK, "project_new", "创建项目", map[string]any{
		"CountLimit": cnt, "SizeLimit": size, "Used": used,
		"Error": "", "Name": "", "Slug": "",
	})
}

func (w *Web) projectNewSubmit(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	st, err := w.st.GetSettings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	slug := normalizeSlug(r.FormValue("slug"))
	newErr := func(msg string) {
		cnt, size := w.quotaFor(u, st)
		used, _, _ := w.st.OwnerUsage(u.ID)
		w.render(rw, r, http.StatusBadRequest, "project_new", "创建项目", map[string]any{
			"CountLimit": cnt, "SizeLimit": size, "Used": used,
			"Error": msg, "Name": name, "Slug": r.FormValue("slug"),
		})
	}

	if name == "" {
		newErr("请填写项目名称。")
		return
	}
	if slug == "" {
		slug = normalizeSlug(name)
	}
	autoSlug := false
	if slug == "" {
		// 中文名称等推导不出合法前缀时，自动生成随机短前缀
		slug = "web-" + strings.ToLower(randCode(6))
		autoSlug = true
	}
	if !validSlug(slug) {
		newErr("子域名前缀只能包含小写字母、数字和短横线，且不能是保留字。")
		return
	}

	cnt, _ := w.quotaFor(u, st)
	used, _, err := w.st.OwnerUsage(u.ID)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if int64(cnt) <= used {
		w.errorPage(rw, r, http.StatusForbidden, "项目数量已达上限（"+strconv.Itoa(cnt)+" 个）。如需更多额度请联系管理员。")
		return
	}

	pid, err := w.st.CreateProject(u.ID, slug, name)
	if err != nil {
		if store.IsUniqueErr(err) {
			newErr("该子域名前缀已被占用，请换一个。")
			return
		}
		log.Printf("[web] 创建项目失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	if err := w.disk.Create(pid); err != nil {
		log.Printf("[web] 创建项目目录失败: %v", err)
		w.st.DeleteProject(pid)
		w.errorPage(rw, r, http.StatusInternalServerError, "创建项目目录失败")
		return
	}
	w.st.Audit(&u.ID, u.Username, "project.create", slug)
	if autoSlug {
		flash(rw, r, "项目已创建，系统自动分配了子域名前缀 "+slug+"，可在项目设置中修改。")
	} else {
		flash(rw, r, "项目已创建，上传 zip 即可发布。")
	}
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(pid, 10), http.StatusSeeOther)
}

// ---------- 项目详情 ----------

func (w *Web) loadProject(rw http.ResponseWriter, r *http.Request, u *store.User) (*store.Project, bool) {
	id := pathID(rw, r, "id")
	if id == 0 {
		return nil, false
	}
	p, err := w.st.GetProject(id)
	if err != nil {
		log.Printf("[web] 查询项目失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return nil, false
	}
	if p == nil {
		w.errorPage(rw, r, http.StatusNotFound, "项目不存在")
		return nil, false
	}
	// 查看权限：所有者、审核员、管理员
	if p.OwnerID != u.ID && !u.IsReviewer() {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限访问该项目")
		return nil, false
	}
	return p, true
}

func (w *Web) projectPage(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	p, ok := w.loadProject(rw, r, u)
	if !ok {
		return
	}
	st, _ := w.st.GetSettings()
	files, size, err := w.disk.Usage(p.ID)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	domainCount := 0
	if domains, err := w.st.ListDomainsByProject(p.ID); err == nil {
		domainCount = len(domains)
	}
	_, sizeLimit := w.quotaFor(u, st)
	w.render(rw, r, http.StatusOK, "project", p.Name, map[string]any{
		"Project":     p,
		"Files":       files,
		"Size":        size,
		"SizeLimit":   sizeLimit,
		"CanManage":   canManageProject(u, p),
		"SitesHost":   st.SitesHost,
		"ReviewOn":    st.ReviewEnabled,
		"DomainCount": domainCount,
	})
}

// ---------- zip 上传 ----------

func (w *Web) projectUpload(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	p, ok := w.loadProject(rw, r, u)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "只有项目所有者或管理员可以上传内容")
		return
	}
	if !w.ensureMutable(rw, r, p) {
		return
	}
	st, err := w.settings()
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	_, sizeLimit := w.quotaFor(u, st)

	// 限制请求体：zip 内容 ≤ sizeLimit，另留 1MB 表单开销
	r.Body = http.MaxBytesReader(rw, r.Body, sizeLimit+(1<<20))
	file, _, err := r.FormFile("zip")
	if err != nil {
		w.errorPage(rw, r, http.StatusBadRequest, "请选择 zip 文件（或文件超出大小限制）。")
		return
	}
	defer file.Close()

	files, size, err := w.disk.ExtractZip(p.ID, file, sizeLimit)
	if err != nil {
		switch err {
		case storage.ErrTooLarge:
			w.errorPage(rw, r, http.StatusBadRequest,
				"解压后超出单项目大小上限（"+byteSize(sizeLimit)+"）。")
		case storage.ErrTooMany:
			w.errorPage(rw, r, http.StatusBadRequest,
				"文件数量超过上限（"+strconv.Itoa(storage.MaxFiles)+" 个）。")
		default:
			w.errorPage(rw, r, http.StatusBadRequest, "上传失败: "+err.Error())
		}
		return
	}
	w.updateProjectSize(p)

	if st.ReviewEnabled {
		if p.Status != store.StatusPending {
			w.st.UpdateProjectStatus(p.ID, store.StatusPending, "", nil)
		}
		w.notifyReviewers("新内容待审核", "项目「"+p.Name+"」("+p.Slug+") 上传了新版本（"+strconv.Itoa(files)+" 个文件，"+byteSize(size)+"），等待审核。")
		flash(rw, r, "上传成功，项目已提交审核。")
	} else {
		w.st.UpdateProjectStatus(p.ID, store.StatusPublished, "", nil)
		flash(rw, r, "上传成功，站点已发布。")
	}
	w.st.Audit(&u.ID, u.Username, "project.upload", p.Slug+" ("+strconv.Itoa(files)+" files, "+strconv.FormatInt(size, 10)+" bytes)")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
}

// ---------- 项目改名 / 删除 ----------

func (w *Web) projectMeta(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	p, ok := w.loadProject(rw, r, u)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限修改该项目")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	slug := normalizeSlug(r.FormValue("slug"))
	if name == "" || slug == "" || !validSlug(slug) {
		w.errorPage(rw, r, http.StatusBadRequest, "名称或子域名前缀不合法。")
		return
	}
	if err := w.st.UpdateProjectMeta(p.ID, name, slug); err != nil {
		if store.IsUniqueErr(err) {
			flash(rw, r, "该子域名前缀已被占用。")
		} else {
			log.Printf("[web] 更新项目失败: %v", err)
			flash(rw, r, "更新失败。")
		}
		http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
		return
	}
	w.st.Audit(&u.ID, u.Username, "project.meta", p.Slug+" -> "+slug)
	flash(rw, r, "项目信息已更新。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
}

func (w *Web) projectDelete(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	p, ok := w.loadProject(rw, r, u)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限删除该项目")
		return
	}
	if r.FormValue("confirm") != p.Slug {
		w.errorPage(rw, r, http.StatusBadRequest, "请输入项目子域名前缀确认删除。")
		return
	}
	if err := w.disk.Destroy(p.ID); err != nil {
		log.Printf("[web] 删除项目目录失败: %v", err)
	}
	if err := w.st.DeleteProject(p.ID); err != nil {
		log.Printf("[web] 删除项目记录失败: %v", err)
		w.errorPage(rw, r, http.StatusInternalServerError, "删除失败")
		return
	}
	w.st.Audit(&u.ID, u.Username, "project.delete", p.Slug)
	flash(rw, r, "项目已删除。")
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

// parentDir 返回路径的父目录（供操作后跳回列表）。
func parentDir(p string) string {
	idx := strings.LastIndex(path.Clean("/"+p), "/")
	if idx <= 0 {
		return "."
	}
	return strings.TrimPrefix(path.Clean("/" + p)[:idx], "/")
}
