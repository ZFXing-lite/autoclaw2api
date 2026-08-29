// main.go autoclaw2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/pool"
	"autoclaw2api/internal/scheduler"
	"autoclaw2api/internal/server"
	"autoclaw2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d %s account(s) from %s", len(auths), cfg.Region, cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	defer p.Flush()
	p.SyncToDir(auths)

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.UserAPIBaseOverride = cfg.Upstream.UserAPIBaseOverride
	up.RelayBaseOverride = cfg.Upstream.RelayBaseOverride
	if cfg.Mode == "relay" {
		up.AutoRelayFallback = false
	}

	sch := scheduler.New(scheduler.Config{
		Pool:       p,
		Upstream:   up,
		Interval:   time.Duration(cfg.Schedule.MaintenanceMinutes) * time.Minute,
		PreRefresh: 10 * time.Minute,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Mode:         cfg.Mode,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("autoclaw2api listening on %s (api_key=%v mode=%s accounts=%d)", cfg.Listen, cfg.APIKey != "", cfg.Mode, len(auths))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
