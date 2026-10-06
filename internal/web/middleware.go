package web

import (
	"context"
	"log"
	"net/http"
	"net/url"
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

		csrf := ""
		if c, err := r.Cookie(cookieCSRF); err == nil {
			csrf = c.Value
		}
		if csrf == "" || len(csrf) < 32 {
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

// csrfGuard 校验所有 POST 请求的 CSRF 令牌。
func (w *Web) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			cookie, err := r.Cookie(cookieCSRF)
			if err != nil || cookie.Value == "" || r.FormValue("_csrf") != cookie.Value {
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
				log.Printf("[web] panic: %v %s%s: %v", r.RemoteAddr, r.Method, r.URL.Path, rec)
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
			log.Printf("[web] %d %s %s (%s)", rec.status, r.Method, r.URL.Path, time.Since(start))
		}
	})
}

// ---------- 渲染与鉴权辅助 ----------

// mustSettings 读取设置，失败时回退默认值（渲染路径不允许因设置读取出错而中断）。
func (w *Web) mustSettings() *store.Settings {
	st, err := w.st.GetSettings()
	if err != nil {
		return store.DefaultSettings()
	}
	return st
}

// render 渲染面板页面并自动填充公共数据。
func (w *Web) render(rw http.ResponseWriter, r *http.Request, code int, tpl, title string, data any) {
	st, err := w.st.GetSettings()
	if err != nil {
		st = store.DefaultSettings()
	}
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
