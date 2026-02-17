package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SkyAerope/Silicon-Proxy/internal/config"
	"github.com/SkyAerope/Silicon-Proxy/internal/health"
	"github.com/SkyAerope/Silicon-Proxy/internal/pool"
	"github.com/SkyAerope/Silicon-Proxy/internal/proxy"
	"github.com/SkyAerope/Silicon-Proxy/internal/source"
	"github.com/SkyAerope/Silicon-Proxy/internal/store"
)

func main() {
	configPath := flag.String("config", "./config.jsonc", "path to config jsonc file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config failed", "error", err)
		os.Exit(1)
	}

	redisStore, err := store.NewRedisStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, cfg.HealthCheck.DeadProxyTTLDur)
	if err != nil {
		logger.Error("init redis failed", "error", err)
		os.Exit(1)
	}
	defer redisStore.Close()

	if err := redisStore.MigrateLegacyDeadSetToTTL(context.Background()); err != nil {
		logger.Warn("migrate legacy dead proxies failed", "error", err)
	}

	mainContext, cancelMain := context.WithCancel(context.Background())
	defer cancelMain()

	healthTarget, err := resolveHealthTarget(cfg.Backend, cfg.HealthCheck.Target)
	if err != nil {
		logger.Error("resolve health check target failed", "error", err)
		os.Exit(1)
	}

	healthChecker := health.NewChecker(
		redisStore,
		logger,
		cfg.HealthCheck.IntervalDur,
		cfg.HealthCheck.TimeoutDur,
		healthTarget,
		cfg.HealthCheck.MaxFailures,
		cfg.HealthCheck.Concurrency,
	)

	sourceManager, err := source.NewManager(cfg, redisStore, logger, func(ctx context.Context) {
		healthChecker.RunOnce(ctx)
	})
	if err != nil {
		logger.Error("init source manager failed", "error", err)
		os.Exit(1)
	}

	sourceManager.Run(mainContext)

	go healthChecker.Run(mainContext)

	transportPool := pool.NewTransportPool(cfg)
	authRouter := pool.NewAuthRouter(redisStore, transportPool, cfg.MaxAuthPerProxy, cfg.HealthCheck.MaxFailures)

	handler, err := proxy.NewHandler(authRouter, cfg.Backend, cfg.MaxRequestRetries, cfg.RequestTimeoutDur, logger)
	if err != nil {
		logger.Error("create proxy handler failed", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("silicon-proxy started", "listen", cfg.Listen, "backend", cfg.Backend)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "error", err)
			cancelMain()
		}
	}()

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-signalChannel:
		logger.Info("shutdown signal received")
	case <-mainContext.Done():
		logger.Info("main context canceled")
	}

	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Warn("http server shutdown failed", "error", err)
	}
}

func resolveHealthTarget(backendURL string, configuredTarget string) (string, error) {
	if strings.TrimSpace(configuredTarget) == "" || strings.EqualFold(strings.TrimSpace(configuredTarget), "backend") {
		parsed, err := url.Parse(backendURL)
		if err != nil {
			return "", err
		}

		if parsed.Host == "" {
			return "", nil
		}

		host := parsed.Host
		_, _, err = net.SplitHostPort(host)
		if err == nil {
			return host, nil
		}

		port := "443"
		if strings.EqualFold(parsed.Scheme, "http") {
			port = "80"
		}
		return net.JoinHostPort(host, port), nil
	}

	return strings.TrimSpace(configuredTarget), nil
}
