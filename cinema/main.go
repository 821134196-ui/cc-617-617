package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"cinema/handler"
	"cinema/models"
	"cinema/seed"
	"cinema/service"
	"cinema/storage"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "data/cinema.db", "sqlite database path")
	ttl := flag.Duration("ttl", 60*time.Second, "seat reservation time-to-live")
	reset := flag.Bool("reset", false, "reset demo data on startup")
	flag.Parse()

	db, err := storage.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err := models.AutoMigrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if *reset {
		if _, err := seed.Reset(db, time.Now()); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Println("demo data reset")
	}

	svc := service.NewExchangeService(db, *ttl)

	// 预留超时释放：每 2 秒扫一次。
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n, err := svc.ExpireOnce(); err != nil {
					log.Printf("expire scan: %v", err)
				} else if n > 0 {
					log.Printf("released %d expired reservations", n)
				}
			case <-stop:
				return
			}
		}
	}()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	h := handler.New(svc)
	h.Register(r)

	r.POST("/api/admin/reset", func(c *gin.Context) {
		id, err := seed.Reset(db, time.Now())
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"member_id": id})
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	r.NoRoute(func(c *gin.Context) {
		// 静态资源/SPA
		if len(c.Request.URL.Path) >= 4 && c.Request.URL.Path[:4] == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		http.FileServer(http.FS(sub)).ServeHTTP(c.Writer, c.Request)
	})

	log.Printf("cinema exchange system listening on %s (ttl=%s)", *addr, ttl)
	if err := r.Run(*addr); err != nil {
		log.Fatal(err)
	}
	close(stop)
}
