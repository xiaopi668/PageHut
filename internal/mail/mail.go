// Package mail 提供最小可用的 SMTP 发信能力（仅标准库）。
package mail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Config 是 SMTP 连接参数（由后台设置提供）。
type Config struct {
	Host string
	Port int
	User string
	Pass string
	From string
	// TLS：starttls（默认，587）| ssl（隐式 TLS，465）| none（明文，仅限内网）
	TLS     string
	Timeout time.Duration
}

// ErrNotConfigured 表示 SMTP 未配置完整。
var ErrNotConfigured = errors.New("SMTP 未配置")

// Send 发送一封纯文本邮件。
func Send(ctx context.Context, cfg Config, to, subject, body string) error {
	if cfg.Host == "" || cfg.From == "" {
		return ErrNotConfigured
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	var (
		conn net.Conn
		err  error
	)
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	if cfg.TLS == "ssl" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer client.Close()

	if cfg.TLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP 服务器不支持 STARTTLS；如需明文请把加密方式改为 none")
		}
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("STARTTLS 失败: %w", err)
		}
	}

	if cfg.User != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("SMTP 服务器不支持认证；请清空用户名或更换服务商")
		}
		// net/smtp 会拒绝在非加密连接上发送明文口令（除非目标是本机）。
		if err := client.Auth(smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}

	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("发件人被拒绝: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("收件人被拒绝: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("开始投递失败: %w", err)
	}
	if _, err := w.Write(buildMessage(cfg.From, to, subject, body)); err != nil {
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("投递失败: %w", err)
	}
	return client.Quit()
}

// buildMessage 组装 RFC 5322 文本邮件（主题按 RFC 2047 编码，避免中文乱码）。
func buildMessage(from, to, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeHeader(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// encodeHeader 对含非 ASCII 的头部做 RFC 2047 Base64 编码。
func encodeHeader(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}
