package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"pagehut/internal/store"
)

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{2,32}$`)
	slugRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	domainRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

// reservedSlugs 是访问路径命名空间中的保留字（避免与面板路由、常见约定冲突）。
var reservedSlugs = map[string]bool{
	"www": true, "panel": true, "admin": true, "api": true, "app": true,
	"static": true, "assets": true, "preview": true, "auth": true,
	"login": true, "logout": true, "register": true, "account": true,
	"projects": true, "review": true, "notifications": true, "mail": true,
	"smtp": true, "ftp": true, "ns1": true, "ns2": true, "root": true,
	"blog": true, "docs": true, "status": true, "help": true,
}

func validUsername(s string) bool { return usernameRe.MatchString(s) }

func validSlug(s string) bool { return slugRe.MatchString(s) && !reservedSlugs[s] }

func validDomain(s string) bool { return len(s) <= 253 && domainRe.MatchString(s) }

// hostOnly 从 Host 头提取纯主机名：小写、去端口、去末尾点，兼容 IPv6。
func hostOnly(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			return h[1:i]
		}
		return ""
	}
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // 系统熵源不可用属于致命错误
	}
	return b
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }

// RandPassword 生成可读的随机密码（供初始管理员使用）。
func RandPassword(n int) string { return randCode(n) }

const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func randCode(n int) string {
	// 直接取模会带来偏置（256 % 31 != 0），这里丢弃落在偏置区间的字节重取。
	limit := 256 - (256 % len(codeAlphabet))
	out := make([]byte, 0, n)
	for len(out) < n {
		for _, c := range randBytes(n) {
			if int(c) >= limit {
				continue
			}
			out = append(out, codeAlphabet[int(c)%len(codeAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}

// ---------- 模板函数 ----------

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func timeFmt(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

func statusName(s string) string {
	switch s {
	case store.StatusPending:
		return "待审核"
	case store.StatusPublished:
		return "已发布"
	case store.StatusRejected:
		return "已驳回"
	case store.StatusSuspended:
		return "已下架"
	}
	return s
}

func statusClass(s string) string {
	switch s {
	case store.StatusPending:
		return "warn"
	case store.StatusPublished:
		return "ok"
	case store.StatusRejected:
		return "bad"
	case store.StatusSuspended:
		return "muted"
	}
	return "muted"
}

func domainStatusName(s string) string {
	switch s {
	case "pending":
		return "待验证"
	case "verified":
		return "已验证待开通"
	case "active":
		return "已生效"
	case "rejected":
		return "已拒绝"
	}
	return s
}

func domainStatusClass(s string) string {
	switch s {
	case "pending":
		return "warn"
	case "verified":
		return "info"
	case "active":
		return "ok"
	case "rejected":
		return "bad"
	}
	return "muted"
}

func registrationModeName(m string) string {
	switch m {
	case "public":
		return "公开注册"
	case "invite":
		return "邀请码注册"
	case "closed":
		return "关闭注册"
	}
	return m
}

func roleName(r string) string {
	switch r {
	case "admin":
		return "管理员"
	case "reviewer":
		return "审核员"
	case "user":
		return "用户"
	}
	return r
}

// ---------- 上下文 ----------

type ctxKey int

const (
	keyUser ctxKey = iota
	keyCSRF
	keyFlash
	keyUnread
)

func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(keyUser).(*store.User)
	return u
}

func csrfFrom(r *http.Request) string {
	s, _ := r.Context().Value(keyCSRF).(string)
	return s
}

func flashFrom(r *http.Request) string {
	s, _ := r.Context().Value(keyFlash).(string)
	return s
}

func unreadFrom(r *http.Request) int64 {
	n, _ := r.Context().Value(keyUnread).(int64)
	return n
}
