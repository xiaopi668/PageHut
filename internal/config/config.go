// Package config 负责 PageHut 的启动配置。
// 运行期可变配置（注册模式、额度、审核开关等）存放在数据库中，
// 由管理员后台修改；这里只保留与部署环境相关的少量选项。
package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// TLSMode 描述 HTTPS 的两种工作模式。
type TLSMode string

const (
	// TLSManual 表示由部署者自己的 Nginx/Caddy 等反代终结 TLS，
	// PageHut 只提供 HTTP 服务。
	TLSManual TLSMode = "manual"
	// TLSAuto 表示 PageHut 内置 ACME（Let's Encrypt）自动签发并续期证书，
	// 需要能够直接占用 80/443 端口。
	TLSAuto TLSMode = "auto"
)

// defaultTrustedProxies 默认只信任本机回环地址：DEPLOY.md 推荐的反代
// （Nginx / Caddy）与 PageHut 同机部署时即命中该默认值。
const defaultTrustedProxies = "127.0.0.1/8,::1/128"

// Config 是解析后的启动配置。
type Config struct {
	DataDir   string  // 数据目录（数据库、站点文件、证书缓存）
	HTTPAddr  string  // manual 模式的主监听地址；auto 模式下为 ACME/跳转端口（通常 :80）
	HTTPSAddr string  // auto 模式的 HTTPS 监听地址（通常 :443）
	TLS       TLSMode // manual | auto
	ACMEEmail string  // ACME 注册邮箱（auto 模式建议填写）
	ACMECache string  // ACME 证书缓存目录

	// TrustedProxies 是允许提供 X-Forwarded-For / X-Real-IP 的直连来源。
	// 只有来自这些地址的请求才会采信转发头，否则一律使用 RemoteAddr。
	TrustedProxies []*net.IPNet
}

// IsTrustedProxy 判断直连对端是否为可信代理。
func (c *Config) IsTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range c.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Parse 解析命令行参数与环境变量（环境变量优先级低于命令行参数）。
func Parse() (*Config, error) {
	c := &Config{}
	var data, tlsMode string
	fs := flag.CommandLine
	fs.StringVar(&data, "data", envOr("PAGEHUT_DATA", "./data"),
		"数据目录（数据库与站点文件）")
	fs.StringVar(&c.HTTPAddr, "http", envOr("PAGEHUT_HTTP", ":8080"),
		"HTTP 监听地址（manual 模式主端口；auto 模式为 ACME/跳转端口，通常 :80）")
	fs.StringVar(&c.HTTPSAddr, "https", envOr("PAGEHUT_HTTPS", ":443"),
		"HTTPS 监听地址（仅 auto 模式）")
	fs.StringVar(&tlsMode, "tls", envOr("PAGEHUT_TLS", "manual"),
		"TLS 模式：manual（外部反代终结 TLS）或 auto（内置 ACME 自动签发）")
	fs.StringVar(&c.ACMEEmail, "acme-email", envOr("PAGEHUT_ACME_EMAIL", ""),
		"ACME 注册邮箱（auto 模式建议填写）")
	fs.StringVar(&c.ACMECache, "acme-cache", envOr("PAGEHUT_ACME_CACHE", ""),
		"ACME 证书缓存目录（默认为 数据目录/acme）")
	trusted := fs.String("trusted-proxies", envOr("PAGEHUT_TRUSTED_PROXIES", defaultTrustedProxies),
		"可信反向代理的 IP 或 CIDR，逗号分隔；只有来自这些地址的 X-Forwarded-For / X-Real-IP 才会被采信")
	flag.Parse()

	switch TLSMode(tlsMode) {
	case TLSManual, TLSAuto:
		c.TLS = TLSMode(tlsMode)
	default:
		return nil, fmt.Errorf("无效的 tls 模式 %q（可选 manual / auto）", tlsMode)
	}

	abs, err := filepath.Abs(data)
	if err != nil {
		return nil, fmt.Errorf("解析数据目录失败: %w", err)
	}
	c.DataDir = abs
	if c.ACMECache == "" {
		c.ACMECache = filepath.Join(c.DataDir, "acme")
	}
	nets, err := parseCIDRList(*trusted)
	if err != nil {
		return nil, err
	}
	c.TrustedProxies = nets
	return c, nil
}

// parseCIDRList 解析逗号分隔的 IP/CIDR 列表（裸 IP 视为 /32 或 /128）。
func parseCIDRList(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, raw := range strings.Split(s, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("无效的可信代理地址 %q", item)
			}
			if ip.To4() != nil {
				item += "/32"
			} else {
				item += "/128"
			}
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("无效的可信代理地址 %q: %w", raw, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
