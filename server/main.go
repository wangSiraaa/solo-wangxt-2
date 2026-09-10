package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type config struct {
	httpAddr     string
	dbDSN        string
	redisAddr    string
	credSecret   string
	reapInterval time.Duration
	webDist      string
	instanceID   string
	leaseDur     time.Duration
	renewEvery   time.Duration
	autoAcquire  bool
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	leaseMs, err := time.ParseDuration(envOr("LEASE_MS", "6000") + "ms")
	if err != nil {
		leaseMs = 6 * time.Second
	}
	return config{
		httpAddr:     envOr("HTTP_ADDR", ":8080"),
		dbDSN:        envOr("DB_DSN", "app:app123@tcp(127.0.0.1:3306)/licensehub?parseTime=true"),
		redisAddr:    envOr("REDIS_ADDR", "127.0.0.1:6379"),
		credSecret:   envOr("CREDENTIAL_SECRET", "dev-only-secret-change-me"),
		reapInterval: 5 * time.Second,
		webDist:      envOr("WEB_DIST", "web/dist"),
		instanceID:   envOr("INSTANCE_ID", "instance-a"),
		leaseDur:     leaseMs,
		renewEvery:   2 * time.Second,
		autoAcquire:  envOr("AUTO_ACQUIRE", "1") != "0",
	}
}

func main() {
	cfg := loadConfig()
	if cfg.credSecret == "dev-only-secret-change-me" {
		log.Println("warn: 使用默认 CREDENTIAL_SECRET,仅限本地演示")
	}

	db, err := sql.Open("mysql", cfg.dbDSN)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(20)
	for i := 0; i < 30; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("等待 MySQL 就绪: %v", err)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}

	store := NewStore(db)
	cache := NewCache(cfg.redisAddr, 0)
	defer cache.Close()

	srv := &Server{
		store: store, cache: cache, cred: []byte(cfg.credSecret), now: time.Now,
		instanceID: cfg.instanceID, view: &AuthorityView{}, leaseDur: cfg.leaseDur,
	}
	r := NewRouter(srv)

	// 租约循环:持有者续租;非持有者在租约过期后接管。
	// 权威判据永远在数据库,这里只是触发;签发路径另有事务内围栏检查兜底。
	go func() {
		ticker := time.NewTicker(cfg.renewEvery)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			now := time.Now()
			held, epoch := srv.view.Get()
			if held {
				ok, err := store.RenewLease(ctx, cfg.instanceID, epoch, cfg.leaseDur, now)
				if err != nil {
					log.Printf("lease renew: %v", err)
				} else if !ok {
					log.Printf("实例 %s 失去权威(epoch %d 已被接管),转为备用", cfg.instanceID, epoch)
					srv.view.Set(false, 0)
				}
			} else if cfg.autoAcquire {
				lease, acquired, err := store.AcquireLease(ctx, cfg.instanceID, "租约过期,自动接管", cfg.leaseDur, now)
				if err != nil {
					log.Printf("lease acquire: %v", err)
				} else if acquired {
					log.Printf("实例 %s 接管权威,epoch=%d", cfg.instanceID, lease.Epoch)
					srv.view.Set(true, lease.Epoch)
				}
			}
			cancel()
		}
	}()

	// 后台过期回收:离线席位到期后自动归还池子(按原有效期,与代次无关)
	go func() {
		ticker := time.NewTicker(cfg.reapInterval)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			n, err := store.ReclaimExpired(ctx, time.Now())
			cancel()
			if err != nil {
				log.Printf("reaper: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("reaper: 回收 %d 个过期离线席位", n)
				cache.Invalidate(context.Background())
			}
		}
	}()

	// 前端静态资源(若已构建)
	if st, err := os.Stat(cfg.webDist); err == nil && st.IsDir() {
		r.Static("/assets", filepath.Join(cfg.webDist, "assets"))
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
				return
			}
			c.File(filepath.Join(cfg.webDist, "index.html"))
		})
		log.Printf("静态资源目录: %s", cfg.webDist)
	} else {
		log.Printf("提示: 未找到 %s,仅提供 API", cfg.webDist)
	}

	log.Printf("LicenseHub 实例 %s 启动: http://localhost%s (租约 %v)", cfg.instanceID, cfg.httpAddr, cfg.leaseDur)
	if err := r.Run(cfg.httpAddr); err != nil {
		log.Fatal(err)
	}
}
