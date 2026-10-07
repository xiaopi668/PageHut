package web

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pagehut/internal/store"
)

// ---------- 用户侧：自定义域名 ----------

func (w *Web) projectDomainsPage(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	st := w.mustSettings()
	domains, err := w.st.ListDomainsByProject(p.ID)
	if err != nil {
		w.errorPage(rw, r, http.StatusInternalServerError, "服务器错误")
		return
	}
	// 只有配置了子域名后缀才有可用的 CNAME 目标；否则给用户一个
	// "<slug>." 这种无法解析的地址，等于把人引到死路上。
	target := ""
	if st.SitesHost != "" {
		target = p.Slug + "." + st.SitesHost
	}
	w.render(rw, r, http.StatusOK, "project_domains", "自定义域名", map[string]any{
		"Project":   p,
		"Domains":   domains,
		"CanManage": canManageProject(u, p),
		"SitesHost": st.SitesHost,
		"Target":    target,
	})
}

// domainRequest 提交自定义域名绑定申请。
func (w *Web) domainRequest(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限修改该项目")
		return
	}
	st := w.mustSettings()

	raw := strings.TrimSpace(r.FormValue("domain"))
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	domain := strings.ToLower(hostOnly(raw))

	fail := func(msg string) {
		flash(rw, r, msg)
		http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/domains", http.StatusSeeOther)
	}

	if !validDomain(domain) {
		fail("域名格式不正确。")
		return
	}
	if domain == st.PanelHost || domain == st.SitesHost ||
		strings.HasSuffix(domain, "."+st.SitesHost) {
		fail("该域名属于系统保留域名，请使用其他域名。")
		return
	}

	token := randHex(16)
	if err := w.st.CreateDomain(p.ID, domain, token); err != nil {
		if store.IsUniqueErr(err) {
			fail("该域名已被申请过。")
			return
		}
		log.Printf("[web] 创建域名申请失败: %v", err)
		fail("服务器错误。")
		return
	}
	w.st.Audit(&u.ID, u.Username, "domain.request", domain+" -> "+p.Slug)
	w.notifyAdmins("新的自定义域名申请", "用户 "+u.Username+" 申请将 "+domain+" 绑定到项目 "+p.Slug+"。")
	flash(rw, r, "申请已提交，请按页面提示完成解析与验证。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/domains", http.StatusSeeOther)
}

// domainCheck 立即检查域名验证状态（CNAME 或 HTTP 文件验证）。
func (w *Web) domainCheck(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限操作该项目")
		return
	}
	did := pathID(rw, r, "did")
	if did == 0 {
		return
	}
	d, err := w.st.GetDomain(did)
	if err != nil || d == nil || d.ProjectID != p.ID {
		w.errorPage(rw, r, http.StatusNotFound, "域名申请不存在")
		return
	}

	fail := func(msg string) {
		flash(rw, r, msg)
		http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/domains", http.StatusSeeOther)
	}

	switch d.Status {
	case "verified":
		fail("该域名已通过验证，等待管理员开通。")
		return
	case "active":
		fail("该域名已生效。")
		return
	case "rejected":
		fail("该域名申请已被拒绝，请重新申请或联系管理员。")
		return
	}

	st := w.mustSettings()
	verified := false
	fetchErr := error(nil)
	fetchStatus := 0

	// 方式一：CNAME 指向 <slug>.<sites_host>（未配置子域名后缀时无此路径）
	if st.SitesHost != "" {
		if canonical, err := net.LookupCNAME(d.Domain); err == nil {
			if strings.TrimSuffix(strings.ToLower(canonical), ".") == p.Slug+"."+st.SitesHost {
				verified = true
			}
		}
	}
	// 方式二：域名 A 记录已指向本服务器，HTTP 令牌验证
	if !verified {
		client := safeHTTPClient(5 * time.Second)
		resp, err := client.Get("http://" + d.Domain + "/.well-known/pagehut-verify/" + d.VerifyToken)
		if err != nil {
			fetchErr = err
		} else {
			fetchStatus = resp.StatusCode
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "验证成功") {
				verified = true
			}
		}
	}

	if !verified {
		// 把失败原因说清楚，否则用户只能看到一句「未检测到解析生效」，
		// 不知道该改 DNS、该开端口，还是该先配子域名后缀。
		switch {
		case fetchErr != nil && errors.Is(fetchErr, errBlockedTarget):
			fail("域名解析到回环 / 链路本地地址，服务端不会向内网发起验证请求；请让域名解析到服务器的公网地址，或改用 CNAME 方式。")
		case fetchErr != nil:
			fail("无法访问 http://" + d.Domain + "/ 的验证地址（连接失败或超时）；请确认解析已生效、80 端口可达。")
		case fetchStatus != http.StatusOK:
			fail("验证地址返回了 HTTP " + strconv.Itoa(fetchStatus) + "，未返回令牌内容；请确认该域名已指向本服务器。")
		case st.SitesHost == "":
			fail("未在该域名上读到验证令牌：请确认 A 记录已指向本服务器；如需用 CNAME 方式，请先在「系统设置 → 域名」配置子域名后缀。")
		default:
			fail("暂未检测到解析生效：请确认 CNAME/A 记录已生效后重试。")
		}
		return
	}
	if err := w.st.UpdateDomainStatus(d.ID, "verified", nil); err != nil {
		log.Printf("[web] 更新域名状态失败: %v", err)
		fail("服务器错误。")
		return
	}
	w.st.Notify(p.OwnerID, "域名验证成功", "域名 "+d.Domain+" 已通过验证，等待管理员开通。")
	w.st.Audit(&u.ID, u.Username, "domain.verify", d.Domain)
	flash(rw, r, "验证成功，等待管理员开通。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/domains", http.StatusSeeOther)
}

// domainDelete 用户删除自己的域名申请。
func (w *Web) domainDelete(rw http.ResponseWriter, r *http.Request) {
	u, p, ok := w.canViewProject(rw, r)
	if !ok {
		return
	}
	if !canManageProject(u, p) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限操作该项目")
		return
	}
	did := pathID(rw, r, "did")
	if did == 0 {
		return
	}
	d, err := w.st.GetDomain(did)
	if err != nil || d == nil || d.ProjectID != p.ID {
		w.errorPage(rw, r, http.StatusNotFound, "域名申请不存在")
		return
	}
	w.st.DeleteDomain(d.ID)
	w.st.Audit(&u.ID, u.Username, "domain.delete", d.Domain)
	flash(rw, r, "域名申请已删除。")
	http.Redirect(rw, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/domains", http.StatusSeeOther)
}
