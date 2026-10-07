// PageHut —— 可自托管的静态网页托管平台。
//
// 启动示例：
//
//	pagehut -data ./data -http :8080                  # HTTP 模式（配合外部反代）
//	pagehut -tls auto -http :80 -https :443           # 内置 ACME 自动 HTTPS
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/bcrypt"

	"pagehut/internal/config"
	"pagehut/internal/db"
	"pagehut/internal/storage"
	"pagehut/internal/store"
	"pagehut/internal/web"
)

// version 可由构建时注入：go build -ldflags "-X main.version=x.y.z"。
var version = "0.1.2"

func main() {
	log.SetFlags(log.LstdFlags)
	cfg, err := config.Parse()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	d, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("%v", err)
	}
	st := store.New(d)
	disk := storage.New(cfg.DataDir)
	// 清理上次异常退出残留的解压 / 交换临时目录（不参与配额统计，会白占磁盘）。
	if n, err := disk.CleanupTemp(); err != nil {
		log.Printf("清理残留临时目录失败: %v", err)
	} else if n > 0 {
		log.Printf("已清理 %d 个残留临时目录", n)
	}

	seedAdmin(st)

	server, err := web.Build(cfg, d, st, disk)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	go sessionCleaner(st)

	// 收到 SIGINT / SIGTERM 后优雅退出：排空在途请求，并关闭数据库
	// 触发 WAL checkpoint（否则 -wal 会长期留着未合并的写入）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var servers []*http.Server
	if cfg.TLS == config.TLSAuto {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Email:      cfg.ACMEEmail,
			Cache:      autocert.DirCache(cfg.ACMECache),
			HostPolicy: server.AutoCertHostPolicy,
		}
		httpsSrv := newServer(cfg.HTTPSAddr, server)
		httpsSrv.TLSConfig = m.TLSConfig()
		// ACME HTTP-01 质询 + 其余跳转 HTTPS
		httpSrv := newServer(cfg.HTTPAddr, m.HTTPHandler(server))
		servers = append(servers, httpsSrv, httpSrv)
		go func() {
			log.Printf("HTTPS 监听 %s（内置 ACME 自动签发）", cfg.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("HTTPS 启动失败: %v", err)
			}
		}()
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("HTTP 启动失败: %v", err)
			}
		}()
		log.Printf("PageHut %s 已启动  TLS=auto  HTTP:%s(ACME/跳转) HTTPS:%s  数据目录:%s",
			version, cfg.HTTPAddr, cfg.HTTPSAddr, cfg.DataDir)
	} else {
		httpSrv := newServer(cfg.HTTPAddr, server)
		servers = append(servers, httpSrv)
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("HTTP 启动失败: %v", err)
			}
		}()
		log.Printf("PageHut %s 已启动  TLS=manual  HTTP:%s  数据目录:%s",
			version, cfg.HTTPAddr, cfg.DataDir)
	}

	<-ctx.Done()
	log.Printf("收到退出信号，正在停止服务…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("关闭监听失败: %v", err)
		}
	}
	if err := d.Close(); err != nil {
		log.Printf("关闭数据库失败: %v", err)
	}
	log.Printf("已安全退出")
}

// newServer 创建带基础超时的 HTTP 服务。
// WriteTimeout 有意不设置：站点可能有大文件下载，硬截止会截断响应。
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       15 * time.Minute, // 容纳 100MB 级 zip 上传
		IdleTimeout:       2 * time.Minute,
	}
}

// seedAdmin 首次启动时创建初始管理员，随机密码打印到日志。
func seedAdmin(st *store.Store) {
	count, err := st.CountUsers()
	if err != nil {
		log.Fatalf("读取用户失败: %v", err)
	}
	if count > 0 {
		return
	}
	password := web.RandPassword(14)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("生成密码失败: %v", err)
	}
	if _, err := st.CreateUser("admin", string(hash), "admin"); err != nil {
		log.Fatalf("创建初始管理员失败: %v", err)
	}
	st.Audit(nil, "system", "system.seed", "创建初始管理员 admin")
	log.Printf("════════════════════════════════════════════════")
	log.Printf("  初始管理员账号: admin")
	log.Printf("  初始管理员密码: %s", password)
	log.Printf("  请登录后立即在「账号设置」中修改密码！")
	log.Printf("════════════════════════════════════════════════")
}

func sessionCleaner(st *store.Store) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if n, err := st.CleanSessions(); err == nil && n > 0 {
			log.Printf("[cron] 清理过期会话 %d 条", n)
		}
	}
}
