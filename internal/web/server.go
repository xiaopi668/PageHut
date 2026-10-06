// Package web 实现 PageHut 的面板与站点 HTTP 服务。
package web

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"pagehut/internal/config"
	"pagehut/internal/sitefs"
	"pagehut/internal/storage"
	"pagehut/internal/store"
)

//go:embed templates/layout.html templates/pages/*.html
var tplFS embed.FS

//go:embed static
var staticFS embed.FS

// Web 是整个 HTTP 服务的根处理器：按 Host 把请求分发给面板或站点。
type Web struct {
	cfg   *config.Config
	st    *store.Store
	disk  *storage.Storage
	tpl   *tplSets
	panel http.Handler
}

// Build 组装面板路由、模板与调度器。
func Build(cfg *config.Config, db *sql.DB, st *store.Store, disk *storage.Storage) (*Web, error) {
	sets, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	w := &Web{cfg: cfg, st: st, disk: disk, tpl: sets}
	panel, err := w.buildPanelMux()
	if err != nil {
		return nil, err
	}
	w.panel = panel
	return w, nil
}

// ---------- Host 调度 ----------

// ServeHTTP 是所有请求的入口：面板、子域名站点、自定义域名、域名验证。
func (w *Web) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	st, err := w.st.GetSettings()
	if err != nil {
		log.Printf("[web] 读取设置失败: %v", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}

	if host == "" || w.isPanelHost(host, st) {
		w.panel.ServeHTTP(rw, r)
		return
	}
	if st.SitesHost != "" && strings.HasSuffix(host, "."+st.SitesHost) {
		slug := strings.TrimSuffix(host, "."+st.SitesHost)
		w.serveBySlug(rw, r, slug)
		return
	}
	if d, _ := w.st.GetDomainByHost(host); d != nil {
		if d.Status == "active" {
			w.serveSite(rw, r, d.ProjectID, store.StatusPublished)
			return
		}
		w.serveDomainVerify(rw, r, d, st)
		return
	}
	serveUnknownHost(rw)
}

func (w *Web) isPanelHost(host string, st *store.Settings) bool {
	if st.PanelHost != "" && host == st.PanelHost {
		return true
	}
	if st.SitesHost != "" && host == st.SitesHost {
		return true
	}
	// 直接用 IP（或 localhost 开发环境）访问时进面板
	if net.ParseIP(host) != nil || host == "localhost" {
		return true
	}
	return false
}

func (w *Web) serveBySlug(rw http.ResponseWriter, r *http.Request, slug string) {
	p, err := w.st.GetProjectBySlug(slug)
	if err != nil {
		log.Printf("[web] 查询项目失败: %v", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}
	if p == nil {
		serveUnknownHost(rw)
		return
	}
	w.serveSite(rw, r, p.ID, p.Status)
}

// serveSite 按项目状态提供站点内容；未发布的站点返回占位页。
func (w *Web) serveSite(rw http.ResponseWriter, r *http.Request, projectID int64, status string) {
	if status == store.StatusPublished {
		sitefs.Handler(w.disk.Dir(projectID)).ServeHTTP(rw, r)
		return
	}
	servePlaceholder(rw, status)
}

// AutoCertHostPolicy 供 autocert 判断哪些 Host 可自动签发证书：
// 面板域名、站点子域名以及已生效的自定义域名。
func (w *Web) AutoCertHostPolicy(_ context.Context, host string) error {
	st, err := w.st.GetSettings()
	if err != nil {
		return err
	}
	if w.isPanelHost(host, st) {
		return nil
	}
	if st.SitesHost != "" && (host == st.SitesHost || strings.HasSuffix(host, "."+st.SitesHost)) {
		return nil
	}
	if d, _ := w.st.GetDomainByHost(host); d != nil && d.Status == "active" {
		return nil
	}
	return errors.New("pagehut: host not managed: " + host)
}

// ---------- 站点侧提示页 ----------

func serveUnknownHost(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusNotFound)
	rw.Write([]byte(unknownHostHTML))
}

const unknownHostHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>404 · PageHut</title><style>body{font-family:system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;background:radial-gradient(46rem 32rem at 12% -8%,rgba(34,211,238,.14),transparent 62%),radial-gradient(50rem 36rem at 90% -4%,rgba(139,92,246,.12),transparent 62%),#07070a;color:#a7a7b3}
.box{text-align:center;padding:2rem}h1{font-size:1.5rem;color:#f2f2f6}code{background:#e5e7eb;padding:.1rem .35rem;border-radius:4px;font-size:.9em}</style></head>
<body><div class="box"><h1>404</h1><p>该域名尚未托管任何站点，或站点不存在。</p><p><small>Powered by PageHut</small></p></div></body></html>`

func servePlaceholder(rw http.ResponseWriter, status string) {
	msg := "该站点正在等待审核，暂时无法访问。"
	switch status {
	case store.StatusRejected:
		msg = "该站点未通过审核，暂时无法访问。"
	case store.StatusSuspended:
		msg = "该站点已被管理员下架。"
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusForbidden)
	tmpl, _ := template.New("p").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>PageHut</title><style>body{font-family:system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;background:radial-gradient(46rem 32rem at 12% -8%,rgba(34,211,238,.14),transparent 62%),radial-gradient(50rem 36rem at 90% -4%,rgba(139,92,246,.12),transparent 62%),#07070a;color:#a7a7b3}
.box{text-align:center;padding:2rem}h1{font-size:1.3rem;color:#e8e8ec}</style></head>
<body><div class="box"><h1>{{.}}</h1><p><small>Powered by PageHut</small></p></div></body></html>`)
	tmpl.Execute(rw, msg)
}

// serveDomainVerify 为待验证的自定义域名提供验证端点与说明页。
func (w *Web) serveDomainVerify(rw http.ResponseWriter, r *http.Request, d *store.Domain, st *store.Settings) {
	tokenPath := "/.well-known/pagehut-verify/" + d.VerifyToken
	if r.URL.Path == tokenPath {
		if d.Status == "pending" {
			if err := w.st.UpdateDomainStatus(d.ID, "verified", nil); err != nil {
				log.Printf("[web] 更新域名状态失败: %v", err)
			} else {
				w.st.Notify(d.OwnerID, "域名验证成功",
					"域名 "+d.Domain+" 已通过所有权验证，请等待管理员开通。")
				w.st.Audit(nil, "system", "domain.verify", d.Domain)
			}
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.Write([]byte("PageHut: 域名验证成功，请返回面板查看状态并等待管理员开通。"))
			return
		}
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.Write([]byte("PageHut: 该域名已通过验证，等待管理员开通。"))
		return
	}

	data := struct {
		Domain, ProjectSlug, SitesHost, Token, Status string
	}{d.Domain, d.ProjectSlug, st.SitesHost, d.VerifyToken, d.Status}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	tmpl, err := template.New("v").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>域名验证 · PageHut</title><style>
body{font-family:system-ui,-apple-system,sans-serif;background:radial-gradient(46rem 32rem at 12% -8%,rgba(34,211,238,.14),transparent 62%),radial-gradient(50rem 36rem at 90% -4%,rgba(139,92,246,.12),transparent 62%),#07070a;color:#a7a7b3;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center}
.box{background:rgba(255,255,255,.055);backdrop-filter:blur(20px) saturate(150%);border:1px solid rgba(255,255,255,.11);border-radius:12px;padding:2rem;max-width:560px;line-height:1.7}
h1{font-size:1.2rem;color:#e8e8ec}code{background:#1b1b1f;border:1px solid #26262b;padding:.1rem .35rem;border-radius:6px;font-size:.92em;color:#c9c9d1;word-break:break-all}
ol{padding-left:1.2rem}li{margin:.4rem 0}</style></head>
<body><div class="box">
<h1>PageHut · 自定义域名验证</h1>
<p>域名 <code>{{.Domain}}</code>（项目 <code>{{.ProjectSlug}}</code>）状态：<b>{{.Status}}</b></p>
<ol>
<li>CNAME 方式：添加解析 <code>{{.Domain}}</code> → CNAME → <code>{{.ProjectSlug}}.{{.SitesHost}}</code></li>
<li>或 A 记录：将 <code>{{.Domain}}</code> 的 A 记录指向本服务器 IP</li>
<li>解析生效后，访问 <code>http://{{.Domain}}/.well-known/pagehut-verify/{{.Token}}</code> 完成所有权验证</li>
<li>验证通过后，管理员将在后台为你开通</li>
</ol>
</div></body></html>`)
	if err == nil {
		tmpl.Execute(rw, data)
	}
}

// ---------- 模板设施 ----------

type tplSets struct {
	sets map[string]*template.Template
}

var templatePages = []string{
	"login", "register", "dashboard", "project_new", "project", "files",
	"fileedit", "review_queue", "review_detail", "settings", "users",
	"user_new", "user_edit", "invites", "project_domains", "admin_domains",
	"audit", "notifications", "account", "error",
}

func loadTemplates() (*tplSets, error) {
	funcs := template.FuncMap{
		"bytesize":             byteSize,
		"timefmt":              timeFmt,
		"statusName":           statusName,
		"statusClass":          statusClass,
		"domainStatusName":     domainStatusName,
		"domainStatusClass":    domainStatusClass,
		"registrationModeName": registrationModeName,
		"roleName":             roleName,
		"parentDirOf":          parentDir,
		"div":                  func(a, b int64) int64 { return a / b },
		"add1":                 func(i int) int { return i + 1 },
		"sub1":                 func(i int) int { return i - 1 },
	}
	base, err := template.New("layout.html").Funcs(funcs).ParseFS(tplFS, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	t := &tplSets{sets: map[string]*template.Template{}}
	for _, p := range templatePages {
		set, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := set.ParseFS(tplFS, "templates/pages/"+p+".html"); err != nil {
			return nil, err
		}
		t.sets[p] = set
	}
	return t, nil
}

// pageData 是所有页面模板共享的顶层结构。
type pageData struct {
	Title    string
	User     *store.User
	CSRF     string
	Flash    string
	Unread   int64
	Settings *store.Settings
	Data     any
}

func (t *tplSets) render(rw http.ResponseWriter, name string, data *pageData) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.sets[name].ExecuteTemplate(rw, "layout.html", data); err != nil {
		log.Printf("[web] 渲染模板 %s 失败: %v", name, err)
	}
}

// ---------- 面板路由 ----------

func (w *Web) buildPanelMux() (http.Handler, error) {
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/{path...}", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// 认证与账号
	mux.HandleFunc("GET /login", w.loginPage)
	mux.HandleFunc("POST /login", w.loginSubmit)
	mux.HandleFunc("GET /register", w.registerPage)
	mux.HandleFunc("POST /register", w.registerSubmit)
	mux.HandleFunc("POST /logout", w.logoutSubmit)
	mux.HandleFunc("GET /account", w.accountPage)
	mux.HandleFunc("POST /account/password", w.accountPasswordSubmit)

	// 通知
	mux.HandleFunc("GET /notifications", w.notificationsPage)
	mux.HandleFunc("POST /notifications/read", w.notificationsRead)

	// 项目
	mux.HandleFunc("GET /{$}", w.dashboardPage)
	mux.HandleFunc("GET /projects/new", w.projectNewPage)
	mux.HandleFunc("POST /projects/new", w.projectNewSubmit)
	mux.HandleFunc("GET /projects/{id}", w.projectPage)
	mux.HandleFunc("POST /projects/{id}/delete", w.projectDelete)
	mux.HandleFunc("POST /projects/{id}/upload", w.projectUpload)
	mux.HandleFunc("POST /projects/{id}/meta", w.projectMeta)
	mux.HandleFunc("GET /projects/{id}/files", w.filesPage)
	mux.HandleFunc("GET /projects/{id}/file", w.fileEditPage)
	mux.HandleFunc("POST /projects/{id}/file/save", w.fileSave)
	mux.HandleFunc("POST /projects/{id}/file/delete", w.fileDelete)
	mux.HandleFunc("POST /projects/{id}/file/mkdir", w.fileMkdir)
	mux.HandleFunc("POST /projects/{id}/file/upload", w.fileUpload)
	mux.HandleFunc("GET /projects/{id}/file/download", w.fileDownload)
	mux.HandleFunc("GET /projects/{id}/domains", w.projectDomainsPage)
	mux.HandleFunc("POST /projects/{id}/domains", w.domainRequest)
	mux.HandleFunc("POST /projects/{id}/domains/{did}/check", w.domainCheck)
	mux.HandleFunc("POST /projects/{id}/domains/{did}/delete", w.domainDelete)

	// 预览（面板同源，带会话）
	mux.HandleFunc("GET /preview/{id}", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/preview/"+r.PathValue("id")+"/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /preview/{id}/{path...}", w.previewHandler)

	// 审核
	mux.HandleFunc("GET /review", w.reviewQueue)
	mux.HandleFunc("GET /review/{id}", w.reviewDetail)
	mux.HandleFunc("POST /review/{id}", w.reviewAction)

	// 管理员
	mux.HandleFunc("GET /admin/settings", w.adminSettingsPage)
	mux.HandleFunc("POST /admin/settings", w.adminSettingsSubmit)
	mux.HandleFunc("GET /admin/users", w.adminUsersPage)
	mux.HandleFunc("GET /admin/users/new", w.adminUserNewPage)
	mux.HandleFunc("POST /admin/users/new", w.adminUserNewSubmit)
	mux.HandleFunc("GET /admin/users/{id}", w.adminUserEditPage)
	mux.HandleFunc("POST /admin/users/{id}", w.adminUserSave)
	mux.HandleFunc("POST /admin/users/{id}/delete", w.adminUserDelete)
	mux.HandleFunc("GET /admin/invites", w.adminInvitesPage)
	mux.HandleFunc("POST /admin/invites", w.adminInviteCreate)
	mux.HandleFunc("POST /admin/invites/delete", w.adminInviteDelete)
	mux.HandleFunc("GET /admin/domains", w.adminDomainsPage)
	mux.HandleFunc("POST /admin/domains/{id}", w.adminDomainAction)
	mux.HandleFunc("GET /admin/audit", w.adminAuditPage)

	handler := http.Handler(mux)
	handler = w.csrfGuard(handler)
	handler = w.sessionLoader(handler)
	handler = w.recoverer(handler)
	handler = w.logger(handler)
	return handler, nil
}

// previewHandler 预览项目站点（面板同源，任何状态都可看，用于审核）。
func (w *Web) previewHandler(rw http.ResponseWriter, r *http.Request) {
	u, ok := w.auth(rw, r)
	if !ok {
		return
	}
	id := pathID(rw, r, "id")
	if id == 0 {
		return
	}
	p, err := w.st.GetProject(id)
	if err != nil || p == nil {
		w.errorPage(rw, r, http.StatusNotFound, "项目不存在")
		return
	}
	if p.OwnerID != u.ID && !u.IsReviewer() {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限预览该项目")
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + r.PathValue("path")
	sitefs.Handler(w.disk.Dir(p.ID)).ServeHTTP(rw, r2)
}

// ---------- 小工具 ----------

// pathID 解析路径中的整型参数，非法时已写好 404 响应。
func pathID(rw http.ResponseWriter, r *http.Request, name string) int64 {
	v := r.PathValue(name)
	var id int64
	for _, c := range v {
		if c < '0' || c > '9' {
			wErr(rw, "not found", http.StatusNotFound)
			return 0
		}
		id = id*10 + int64(c-'0')
		if id > 1<<40 {
			wErr(rw, "not found", http.StatusNotFound)
			return 0
		}
	}
	if id <= 0 {
		wErr(rw, "not found", http.StatusNotFound)
		return 0
	}
	return id
}

func wErr(rw http.ResponseWriter, msg string, code int) {
	http.Error(rw, msg, code)
}

// safeNext 校验登录后的跳转地址，只允许站内路径。
func safeNext(s string) string {
	if s == "" || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return "/"
	}
	if u, err := url.Parse(s); err != nil || u.Host != "" {
		return "/"
	}
	return s
}
