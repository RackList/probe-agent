package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Everything the operator sets. Deliberately short: what to measure and how
// often are network-wide decisions that come from the server, not from here.
// A probe that could pick its own targets could pick a target it has an
// interest in, which is the one thing the network is built to prevent.
type config struct {
	Token string
	API   string

	// Per-request budgets. Local concerns: they depend on the machine and its
	// link, not on the network.
	ProbeTimeout  time.Duration
	SubmitTimeout time.Duration

	// Skip TLS verification. For pointing an agent at a development instance
	// with a self-signed certificate; never for production use.
	Insecure bool
}

const (
	defaultProbeTimeout  = 10 * time.Second
	defaultSubmitTimeout = 15 * time.Second
)

func loadConfig() (*config, error) {
	cfg := &config{
		Token:         strings.TrimSpace(os.Getenv("PROBE_TOKEN")),
		API:           strings.TrimRight(strings.TrimSpace(os.Getenv("PROBE_API")), "/"),
		ProbeTimeout:  defaultProbeTimeout,
		SubmitTimeout: defaultSubmitTimeout,
		Insecure:      envBool("PROBE_INSECURE"),
	}

	if cfg.Token == "" {
		return nil, fmt.Errorf("PROBE_TOKEN is required (the pb_ token shown once when you enrolled the probe)")
	}
	if !strings.HasPrefix(cfg.Token, "pb_") {
		return nil, fmt.Errorf("PROBE_TOKEN does not look like a probe token: it should start with pb_")
	}
	if cfg.API == "" {
		return nil, fmt.Errorf("PROBE_API is required (for example https://racklist.eu/api/v1/probe)")
	}

	parsed, err := url.Parse(cfg.API)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("PROBE_API must be an absolute URL, got %q", cfg.API)
	}
	if parsed.Scheme != "https" && !cfg.Insecure {
		return nil, fmt.Errorf("PROBE_API must use https (set PROBE_INSECURE=true only against a development instance)")
	}

	if d, err := envDuration("PROBE_TIMEOUT"); err != nil {
		return nil, err
	} else if d > 0 {
		cfg.ProbeTimeout = d
	}

	if d, err := envDuration("PROBE_SUBMIT_TIMEOUT"); err != nil {
		return nil, err
	} else if d > 0 {
		cfg.SubmitTimeout = d
	}

	return cfg, nil
}

func (c *config) configURL() string { return c.API + "/config" }

func (c *config) measurementsURL() string { return c.API + "/measurements" }

func (c *config) calibrationURL() string { return c.API + "/calibration" }

// Accepts "30s", "5m", or a bare integer read as seconds, so an operator
// copying a number out of a shell script is not punished for it.
func envDuration(key string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}

	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
		}
		return time.Duration(seconds) * time.Second, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a duration: %q", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
	}

	return d, nil
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
