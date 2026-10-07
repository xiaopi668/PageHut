package web

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// errBlockedTarget 表示目标地址属于不允许访问的范围。
var errBlockedTarget = errors.New("目标地址不允许访问")

// safeHTTPClient 返回用于「访问用户提交的域名」的 HTTP 客户端。
//
// 域名验证要主动去请求用户填写的域名（handlers_domains.go 的 A 记录 +
// 令牌方式），这天然是一个 SSRF 面：攻击者可以让自己的域名 302 到任意
// 地址。这里做两层收敛：
//   - 连接前校验解析出来的 IP，拒绝回环 / 链路本地 / 未指定 / 组播地址
//     （云元数据服务 169.254.169.254 属于链路本地，会被拦下）；
//   - 最多跟随 2 次跳转。
//
// 私有网段（RFC1918）故意放行：自托管场景下用户域名常常就解析到内网地址，
// 一律封禁会让内网部署无法完成验证。
func safeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errBlockedTarget
			}
			ip := net.ParseIP(host)
			if ip == nil || !allowedTarget(ip) {
				return errBlockedTarget
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			DisableKeepAlives:     true,
			MaxIdleConns:          2,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 2 {
				return errors.New("跳转次数过多")
			}
			return nil
		},
	}
}

// allowedTarget 判断解析后的地址是否允许连接。
func allowedTarget(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), ip.IsUnspecified(), ip.IsMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast():
		return false
	}
	return true
}
