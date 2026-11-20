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

// main 只负责装配依赖与生命周期：HTTP 接收请求，Worker 异步建单，定时任务取消过期订单。
// 核心业务链路可从 service.Seckill → Gateway.Reserve → Worker.Process → Store.HandleReservation 阅读。
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
	// 启动只确保消费组存在，不用 DB 库存覆盖 Redis，避免把仍在排队的预扣库存重新放出。
	if err := gate.EnsureGroup(context.Background()); err != nil {
		logger.Error("ensure redis stream group failed", "error", err)
		os.Exit(1)
	}

	svc := service.New(store, gate, cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var workerReady atomic.Bool
	var wg sync.WaitGroup
	// 这里只启动一个订单 Worker；其内部批量读取消息后串行落库。
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

	// 就绪检查反映 Worker 循环、依赖连接及消费组状态，不代表所有商品库存均就绪或积压已清空。
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
	// HTTP 尝试等待在途请求完成，随后等待后台任务退出，再由 defer 关闭连接。
	// 信号已会取消共享 ctx，因此当前 Worker/取消任务可能先于 HTTP 排空停止。
	_ = server.Shutdown(shutdownCtx)
	stop()
	wg.Wait()
}

// runCanceller 定期扫描已落库且过期的待支付订单，不能用它回补“超时但查不到订单”的预约。
// 每轮先在 DB 事务中取消和归还库存，再由 Service 尝试回补 Redis。
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
