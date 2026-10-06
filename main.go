// PageHut —— 可自托管的静态网页托管平台。
//
// 启动示例：
//
//	pagehut -data ./data -http :8080                  # HTTP 模式（配合外部反代）
//	pagehut -tls auto -http :80 -https :443           # 内置 ACME 自动 HTTPS
package main

import (
	"log"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/bcrypt"

	"pagehut/internal/config"
	"pagehut/internal/db"
	"pagehut/internal/storage"
	"pagehut/internal/store"
	"pagehut/internal/web"
)

const version = "0.1.0"

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

	seedAdmin(st)

	server, err := web.Build(cfg, d, st, disk)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	go sessionCleaner(st)

	if cfg.TLS == config.TLSAuto {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Email:      cfg.ACMEEmail,
			Cache:      autocert.DirCache(cfg.ACMECache),
			HostPolicy: server.AutoCertHostPolicy,
		}
		httpsSrv := &http.Server{
			Addr:              cfg.HTTPSAddr,
			Handler:           server,
			TLSConfig:         m.TLSConfig(),
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			log.Printf("HTTPS 监听 %s（内置 ACME 自动签发）", cfg.HTTPSAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil {
				log.Fatalf("HTTPS 启动失败: %v", err)
			}
		}()
		httpSrv := &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           m.HTTPHandler(server), // ACME HTTP-01 质询 + 其余跳转 HTTPS
			ReadHeaderTimeout: 15 * time.Second,
		}
		log.Printf("PageHut %s 已启动  TLS=auto  HTTP:%s(ACME/跳转) HTTPS:%s  数据目录:%s",
			version, cfg.HTTPAddr, cfg.HTTPSAddr, cfg.DataDir)
		log.Fatal(httpSrv.ListenAndServe())
	}

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("PageHut %s 已启动  TLS=manual  HTTP:%s  数据目录:%s",
		version, cfg.HTTPAddr, cfg.DataDir)
	log.Fatal(httpSrv.ListenAndServe())
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
