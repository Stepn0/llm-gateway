// Command llm-gateway is a reverse proxy for OpenAI-compatible LLM APIs with
// per-key rate limiting, response caching, retries, circuit breaking and
// multi-provider fallback.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Stepn0/llm-gateway/internal/breaker"
	"github.com/Stepn0/llm-gateway/internal/cache"
	"github.com/Stepn0/llm-gateway/internal/config"
	"github.com/Stepn0/llm-gateway/internal/metrics"
	"github.com/Stepn0/llm-gateway/internal/proxy"
	"github.com/Stepn0/llm-gateway/internal/ratelimit"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(*configPath, logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := proxy.Options{
		Registry:    metrics.NewRegistry(),
		Logger:      logger,
		MaxAttempts: cfg.Retry.MaxAttempts,
		BaseBackoff: cfg.Retry.BaseBackoff(),
		MaxBackoff:  cfg.Retry.MaxBackoff(),
	}
	if cfg.RateLimit.RPS > 0 {
		opts.Limiter = ratelimit.New(cfg.RateLimit.RPS, cfg.RateLimit.Burst)
		go opts.Limiter.RunJanitor(ctx, time.Minute, cfg.RateLimit.IdleEvict())
	}
	if cfg.Cache.Size > 0 {
		opts.Cache = cache.New[proxy.Cached](cfg.Cache.Size, cfg.Cache.TTL())
	}
	for _, p := range cfg.Providers {
		opts.Upstreams = append(opts.Upstreams, proxy.Upstream{
			Name:    p.Name,
			BaseURL: p.BaseURL,
			APIKey:  p.APIKey,
			Timeout: p.Timeout(),
			Breaker: breaker.New(cfg.Breaker.FailureThreshold, cfg.Breaker.OpenFor()),
		})
		logger.Info("upstream configured", "name", p.Name, "baseURL", p.BaseURL, "auth", p.APIKeyEnv != "")
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", proxy.New(opts))
	mux.Handle("GET /metrics", opts.Registry.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "providers", len(cfg.Providers))
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
