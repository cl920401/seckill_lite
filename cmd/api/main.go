package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"seckill_lite/internal/config"
	"seckill_lite/internal/httpapi"
	"seckill_lite/internal/mysqlrepo"
	"seckill_lite/internal/redisrepo"
	"seckill_lite/internal/service"
	"seckill_lite/internal/worker"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store, err := mysqlrepo.Open(cfg.MySQLDSN, cfg.OrderExpire)
	if err != nil {
		logger.Error("open mysql failed", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	gate := redisrepo.New(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.RedisPrefix, cfg.StreamKey, cfg.StreamGroup, cfg.RestockTTL)
	defer gate.Close()
	if err := gate.EnsureGroup(context.Background()); err != nil {
		logger.Error("ensure redis stream group failed", "error", err)
		os.Exit(1)
	}

	svc := service.New(store, gate, cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var workerReady atomic.Bool
	var wg sync.WaitGroup
	wk := worker.New(store, gate, cfg.ClaimIdle)
	wg.Add(1)
	go func() {
		defer wg.Done()
		workerReady.Store(true)
		defer workerReady.Store(false)
		logger.Info("order worker started")
		wk.Run(ctx)
		logger.Info("order worker stopped")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		runCanceller(ctx, svc, cfg, logger)
	}()

	readiness := func(ctx context.Context) error {
		if cfg.Readiness && !workerReady.Load() {
			return errors.New("worker is not running")
		}
		if err := store.Ping(ctx); err != nil {
			return err
		}
		if err := gate.Ping(ctx); err != nil {
			return err
		}
		ok, err := gate.GroupExists(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("stream consumer group is missing")
		}
		return nil
	}

	handler := httpapi.New(svc, cfg, readiness)
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			stop()
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	stop()
	wg.Wait()
}

func runCanceller(ctx context.Context, svc *service.Service, cfg config.Config, logger *slog.Logger) {
	interval := cfg.CancelInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			n, err := svc.CancelExpired(cancelCtx)
			cancel()
			if err != nil {
				logger.Error("cancel expired orders failed", "error", err)
			} else if n > 0 {
				logger.Info("cancelled expired orders", "count", n)
			}
		}
	}
}
