// Command probe-agent is the RackList community probe.
//
// It asks the server what to measure, measures it, and reports back. It never
// picks its own targets: the pool is drawn server-side and rotates, which is
// what makes an arrangement between a probe and a target impossible to set up.
//
// Configuration is three environment variables at most; see the README.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "1.0.0"

const (
	// Retry budget for one round. Beyond that the round is dropped: the next
	// one is a few minutes away and carries fresher data anyway.
	maxSubmitAttempts = 4
	initialBackoff    = 2 * time.Second
	maxBackoff        = 60 * time.Second

	// How long to wait before retrying a failed configuration fetch. Without a
	// pool there is nothing to measure, so this is the whole loop.
	configRetryDelay = 60 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, newAPIClient(cfg), logger); err != nil {
		logger.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config, api *apiClient, logger *slog.Logger) error {
	logger.Info("probe agent starting", "version", version, "api", cfg.API)

	serverCfg, err := fetchConfigWithRetry(ctx, api, logger)
	if err != nil {
		return err
	}
	logStartup(logger, serverCfg)

	measureTicker := time.NewTicker(serverCfg.interval())
	defer measureTicker.Stop()

	refreshTicker := time.NewTicker(serverCfg.refreshInterval())
	defer refreshTicker.Stop()

	runRound(ctx, cfg, api, serverCfg, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown requested, exiting")
			return nil

		case <-refreshTicker.C:
			// The imposed pool rotates. An agent that never re-reads it drifts
			// onto pairs the server has already released.
			fresh, err := api.fetchConfig(ctx)
			if err != nil {
				logger.Warn("could not refresh the pool, keeping the current one", "err", err)
				continue
			}

			if fresh.interval() != serverCfg.interval() {
				measureTicker.Reset(fresh.interval())
			}
			if fresh.refreshInterval() != serverCfg.refreshInterval() {
				refreshTicker.Reset(fresh.refreshInterval())
			}

			serverCfg = fresh
			logger.Info("pool refreshed", "targets", len(serverCfg.Targets))

		case <-measureTicker.C:
			runRound(ctx, cfg, api, serverCfg, logger)
		}
	}
}

func runRound(ctx context.Context, cfg *config, api *apiClient, serverCfg *serverConfig, logger *slog.Logger) {
	if len(serverCfg.Targets) == 0 {
		logger.Info("no target assigned yet, nothing to measure this round")
		return
	}

	samples := measureAll(ctx, cfg, serverCfg.Targets)
	if len(samples) == 0 {
		return
	}

	for _, batch := range chunk(samples, serverCfg.batchSize()) {
		if err := submitWithBackoff(ctx, api, batch, logger); err != nil {
			logger.Error("batch dropped", "samples", len(batch), "err", err)
		}
	}
}

func submitWithBackoff(ctx context.Context, api *apiClient, batch []sample, logger *slog.Logger) error {
	backoff := initialBackoff

	for attempt := 1; attempt <= maxSubmitAttempts; attempt++ {
		result, err := api.submit(ctx, batch)
		if err == nil {
			logger.Info("batch submitted", "stored", result.Data.Stored, "duplicates", result.Data.Duplicates)
			return nil
		}

		// A refused token or a rejected payload will be refused identically on
		// the next try. Retrying would only burn the rate limit budget.
		var permanent *permanentError
		if errors.As(err, &permanent) {
			return err
		}

		if attempt == maxSubmitAttempts {
			return err
		}

		logger.Warn("submit failed, retrying", "attempt", attempt, "in", backoff, "err", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	return nil
}

func fetchConfigWithRetry(ctx context.Context, api *apiClient, logger *slog.Logger) (*serverConfig, error) {
	for {
		serverCfg, err := api.fetchConfig(ctx)
		if err == nil {
			return serverCfg, nil
		}

		var permanent *permanentError
		if errors.As(err, &permanent) {
			return nil, err
		}

		logger.Warn("could not reach the server, retrying", "in", configRetryDelay, "err", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(configRetryDelay):
		}
	}
}

func logStartup(logger *slog.Logger, serverCfg *serverConfig) {
	logger.Info("registered with the network",
		"probe_id", serverCfg.Probe.ID,
		"label", serverCfg.Probe.Label,
		"status", serverCfg.Probe.Status,
		"targets", len(serverCfg.Targets),
		"interval", serverCfg.interval(),
	)
}

func chunk(samples []sample, size int) [][]sample {
	if size <= 0 || len(samples) <= size {
		return [][]sample{samples}
	}

	var out [][]sample
	for start := 0; start < len(samples); start += size {
		end := start + size
		if end > len(samples) {
			end = len(samples)
		}
		out = append(out, samples[start:end])
	}

	return out
}
