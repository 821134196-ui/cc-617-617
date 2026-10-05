package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed all:web/dist
var webAssets embed.FS

func main() {
	dbPath := flag.String("db", "data/cinema.db", "SQLite 数据库路径")
	addr := flag.String("addr", ":8080", "监听地址")
	ttlSec := flag.Int("ttl", 120, "新座位预留秒数")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	svc := NewService(db, time.Duration(*ttlSec)*time.Second)

	// 预留超时回收：每 2 秒扫描一次
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if n, err := svc.ExpireDue(); err != nil {
					log.Printf("超时回收失败: %v", err)
				} else if n > 0 {
					log.Printf("超时回收 %d 个预留，新座位已释放", n)
				}
			case <-stop:
				return
			}
		}
	}()
	defer close(stop)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	registerRoutes(r, &Handler{svc: svc, db: db})

	// 静态前端（Vue 选座页）
	dist, err := fs.Sub(webAssets, "web/dist")
	if err != nil {
		log.Fatal(err)
	}
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if len(p) >= 4 && p[:4] == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		// SPA 回退
		f, err := dist.Open(p)
		if err == nil {
			f.Close()
			http.FileServer(http.FS(dist)).ServeHTTP(c.Writer, c.Request)
			return
		}
		c.Request.URL.Path = "/"
		http.FileServer(http.FS(dist)).ServeHTTP(c.Writer, c.Request)
	})

	log.Printf("电影院会员券改签系统已启动: http://localhost%s （预留 TTL=%ds）", *addr, *ttlSec)
	if err := r.Run(*addr); err != nil {
		log.Fatal(err)
	}
}
