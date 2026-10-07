package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pagehut/internal/store"
)

func contextWith(ctx context.Context, k ctxKey, v any) context.Context {
	return context.WithValue(ctx, k, v)
}

const (
	cookieSession = "pp_session"
	cookieCSRF    = "pp_csrf"
	cookieFlash   = "pp_flash"

	sessionTTL = 7 * 24 * time.Hour
)

func setCookie(rw http.ResponseWriter, name, val string, maxAge int, httpOnly bool, secure bool) {
	http.SetCookie(rw, &http.Cookie{
		Name:     name,
		Value:    val,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// isSecureRequest 判断当前请求是否走 HTTPS（直连或反代转发）。
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// ---------- 客户端 IP（可信代理） ----------

// clientIP 返回真实客户端 IP：只有当直连对端是可信代理（-trusted-proxies）时
// 才采信 X-Forwarded-For / X-Real-IP，否则一律使用 RemoteAddr。
// XFF 从右往左取第一个「非可信代理」地址，避免客户端伪造左侧条目。
func (w *Web) clientIP(r *http.Request) string {
	peer := remoteHost(r.RemoteAddr)
	peerIP := net.ParseIP(peer)
	if peerIP == nil || !w.cfg.IsTrustedProxy(peerIP) {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			item := strings.TrimSpace(parts[i])
			ip := net.ParseIP(item)
			if ip == nil {
				continue
			}
			if !w.cfg.IsTrustedProxy(ip) {
				return item
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return peer
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// ---------- CSRF 令牌 ----------

// csrfTokenFor 为已登录用户派生稳定的 CSRF 令牌：服务端只认这个派生值，
// 不采信 cookie 里写了什么，于是「往父域投放一个自己知道的 pp_csrf」不再
// 能绕过校验。
//
// 令牌绑定「用户 + 持久化密钥」而不是单次会话：进程重启（重新部署）或
// 重新登录后，之前打开的页面里的令牌依然有效，不会出现「点退出没反应」。
// 匿名请求（登录/注册页）没有用户可绑定，沿用随机值 + 双提交。
func (w *Web) csrfTokenFor(userID int64) string {
	if userID <= 0 {
		return ""
	}
	mac := hmac.New(sha256.New, w.csrfKey)
	mac.Write([]byte("pagehut-csrf\x00"))
	mac.Write([]byte(strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------- 安全响应头 ----------

// securityHeaders 给面板响应统一加上安全响应头。
// /preview/ 例外：它承载的是用户上传的任意 HTML，用 CSP sandbox 把它降级为
// 不透明源（脚本照常运行，但读不到面板源的 DOM/Cookie，fetch 面板接口也会被
// CORS 拦下），因此不能加 X-Frame-Options，否则审核页的 iframe 无法加载。
func (w *Web) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/preview/") {
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy",
				"sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads")
		} else {
			h.Set("Referrer-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
			h.Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
					"script-src 'self'; object-src 'none'; base-uri 'self'; "+
					"form-action 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(rw, r)
	})
}

// sessionLoader 加载会话用户，并准备 CSRF 与闪存消息。
func (w *Web) sessionLoader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var user *store.User
		if c, err := r.Cookie(cookieSession); err == nil && c.Value != "" {
			u, err := w.st.GetUserBySession(c.Value)
			if err != nil {
				log.Printf("[web] 查询会话失败: %v", err)
			} else if u != nil && u.Status == "active" {
				user = u
			}
		}

		// CSRF：已登录用户使用「用户派生值」，并把 cookie 同步成同一个值；
		// 匿名用户沿用随机值双提交。
		csrf := ""
		if c, err := r.Cookie(cookieCSRF); err == nil {
			csrf = c.Value
		}
		if user != nil {
			if want := w.csrfTokenFor(user.ID); want != csrf {
				csrf = want
				setCookie(rw, cookieCSRF, csrf, int(30*24*time.Hour/time.Second), true, isSecureRequest(r))
			}
		} else if csrf == "" || len(csrf) < 32 {
			csrf = randHex(32)
			setCookie(rw, cookieCSRF, csrf, int(30*24*time.Hour/time.Second), true, isSecureRequest(r))
		}

		flash := ""
		if c, err := r.Cookie(cookieFlash); err == nil && c.Value != "" {
			if decoded, derr := url.QueryUnescape(c.Value); derr == nil {
				flash = decoded
			} else {
				flash = c.Value
			}
			setCookie(rw, cookieFlash, "", -1, true, isSecureRequest(r))
		}

		var unread int64
		if user != nil {
			unread, _ = w.st.UnreadCount(user.ID)
		}

		ctx := r.Context()
		ctx = contextWith(ctx, keyUser, user)
		ctx = contextWith(ctx, keyCSRF, csrf)
		ctx = contextWith(ctx, keyFlash, flash)
		ctx = contextWith(ctx, keyUnread, unread)
		next.ServeHTTP(rw, r.WithContext(ctx))
	})
}

// csrfGuard 校验所有 POST 请求的 CSRF 令牌：与「服务端期望值」比较，
// 而不是与请求里的 cookie 比较（后者可被父域 cookie 投放伪造）。
func (w *Web) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			want, _ := r.Context().Value(keyCSRF).(string)
			got := r.FormValue("_csrf")
			if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				http.Error(rw, "CSRF 校验失败，请刷新页面重试", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// recoverer 把 handler panic 转成 500 页面。
func (w *Web) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[web] panic: %v %s%s: %v", w.clientIP(r), r.Method, r.URL.Path, rec)
				http.Error(rw, "服务器内部错误", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (w *Web) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: rw, status: 200}
		next.ServeHTTP(rec, r)
		// 面板请求记日志；站点静态请求由 sitefs 处理，不经过这里
		if rec.status >= 500 {
			log.Printf("[web] %d %s %s (%s) ip=%s", rec.status, r.Method, r.URL.Path, time.Since(start), w.clientIP(r))
		}
	})
}

// ---------- 渲染与鉴权辅助 ----------

// mustSettings 读取设置（带缓存），失败时回退默认值（渲染路径不允许因设置读取出错而中断）。
func (w *Web) mustSettings() *store.Settings {
	st, err := w.settings()
	if err != nil {
		return store.DefaultSettings()
	}
	return st
}

// render 渲染面板页面并自动填充公共数据。
func (w *Web) render(rw http.ResponseWriter, r *http.Request, code int, tpl, title string, data any) {
	st := w.mustSettings()
	pd := &pageData{
		Title:    title,
		User:     userFrom(r),
		CSRF:     csrfFrom(r),
		Flash:    flashFrom(r),
		Unread:   unreadFrom(r),
		Settings: st,
		Data:     data,
	}
	rw.WriteHeader(code)
	w.tpl.render(rw, tpl, pd)
}

// errorPage 输出统一的错误提示页。
func (w *Web) errorPage(rw http.ResponseWriter, r *http.Request, code int, msg string) {
	w.render(rw, r, code, "error", "提示", map[string]any{
		"Code": code,
		"Msg":  msg,
	})
}

// auth 校验登录与角色；未通过时已写好响应，调用方直接 return。
func (w *Web) auth(rw http.ResponseWriter, r *http.Request, roles ...string) (*store.User, bool) {
	u := userFrom(r)
	if u == nil {
		if r.Method == http.MethodGet {
			http.Redirect(rw, r, "/login?next="+r.URL.RequestURI(), http.StatusSeeOther)
		} else {
			http.Error(rw, "请先登录", http.StatusUnauthorized)
		}
		return nil, false
	}
	if len(roles) > 0 && !containsStr(roles, u.Role) {
		w.errorPage(rw, r, http.StatusForbidden, "没有权限访问该页面")
		return nil, false
	}
	return u, true
}

// canManageProject 判断用户是否可以修改该项目（所有者或管理员）。
func canManageProject(u *store.User, p *store.Project) bool {
	return u.IsAdmin() || p.OwnerID == u.ID
}

// flash 设置闪存消息（下一次 GET 渲染时显示）。Cookie 值需 URL 编码以支持中文。
func flash(rw http.ResponseWriter, r *http.Request, msg string) {
	setCookie(rw, cookieFlash, url.QueryEscape(msg), 60, true, isSecureRequest(r))
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
