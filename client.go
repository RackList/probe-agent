package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Points at the public programme page: an administrator who finds these
// requests in their logs can tell what measures them, and ask to be left out.
const userAgent = "racklist-probe-agent/" + version + " (+https://racklist.eu/en/network)"

// What the server tells this probe to do. The pool and the cadence both live
// here rather than in local configuration: they are network-wide parameters,
// and a fleet that paced itself could never be retuned.
type serverConfig struct {
	Probe struct {
		ID     int64  `json:"id"`
		Label  string `json:"label"`
		Status string `json:"status"`
	} `json:"probe"`
	MeasurementIntervalSeconds int      `json:"measurement_interval_seconds"`
	ConfigRefreshSeconds       int      `json:"config_refresh_seconds"`
	MaxBatchSize               int      `json:"max_batch_size"`
	Targets                    []target `json:"targets"`

	// Set while the server still needs to work out where this probe is. The
	// anchors are reference probes whose positions RackList knows; timing the
	// round trip to them is what places this one, since latency is bounded by
	// the speed of light and a database entry is not.
	CalibrationRequired bool     `json:"calibration_required"`
	Anchors             []anchor `json:"anchors"`
}

type target struct {
	ID   int64  `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

// A reference probe to time. Only the endpoint travels: the server keeps the
// certified coordinates, so no agent can work out which answer would place it
// where it would like to be.
type anchor struct {
	ID       int64  `json:"id"`
	Endpoint string `json:"endpoint"`
}

func (c serverConfig) interval() time.Duration {
	if c.MeasurementIntervalSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.MeasurementIntervalSeconds) * time.Second
}

func (c serverConfig) refreshInterval() time.Duration {
	if c.ConfigRefreshSeconds <= 0 {
		return time.Hour
	}
	return time.Duration(c.ConfigRefreshSeconds) * time.Second
}

func (c serverConfig) batchSize() int {
	if c.MaxBatchSize <= 0 {
		return 100
	}
	return c.MaxBatchSize
}

type apiClient struct {
	cfg  *config
	http *http.Client
}

func newAPIClient(cfg *config) *apiClient {
	return &apiClient{
		cfg: cfg,
		http: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure}, //nolint:gosec // guarded by config validation
			},
			Timeout: cfg.SubmitTimeout,
		},
	}
}

func (a *apiClient) fetchConfig(ctx context.Context) (*serverConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.configURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("build config request: %w", err)
	}
	a.setHeaders(req)

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch config: %w", err)
	}
	defer drainAndClose(resp)

	if err := errorForStatus(resp); err != nil {
		return nil, err
	}

	var envelope struct {
		Data serverConfig `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	return &envelope.Data, nil
}

type submitResult struct {
	Data struct {
		Stored     int `json:"stored"`
		Duplicates int `json:"duplicates"`
	} `json:"data"`
}

func (a *apiClient) submit(ctx context.Context, batch []sample) (*submitResult, error) {
	body, err := json.Marshal(map[string]any{"measurements": batch})
	if err != nil {
		return nil, fmt.Errorf("encode batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.measurementsURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build submit request: %w", err)
	}
	a.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("submit: %w", err)
	}
	defer drainAndClose(resp)

	if err := errorForStatus(resp); err != nil {
		return nil, err
	}

	var result submitResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode submit response: %w", err)
	}

	return &result, nil
}

type calibrationResult struct {
	Data struct {
		AcceptedObservations int     `json:"accepted_observations"`
		AccuracyKm           float64 `json:"accuracy_km"`
		Confidence           string  `json:"confidence"`
	} `json:"data"`
}

func (a *apiClient) submitCalibration(ctx context.Context, observations []observation) (*calibrationResult, error) {
	body, err := json.Marshal(map[string]any{"observations": observations})
	if err != nil {
		return nil, fmt.Errorf("encode observations: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.calibrationURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build calibration request: %w", err)
	}
	a.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("submit calibration: %w", err)
	}
	defer drainAndClose(resp)

	if err := errorForStatus(resp); err != nil {
		return nil, err
	}

	var result calibrationResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode calibration response: %w", err)
	}

	return &result, nil
}

func (a *apiClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
}

// permanentError marks a response the agent must not retry: retrying a refused
// token or a rejected payload only burns the rate limit and hides the cause.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

func errorForStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Read a bounded slice of the body: it carries the reason, and an
	// unbounded read on an error path is a denial of service waiting to happen.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &permanentError{msg: "the server rejected the token: check PROBE_TOKEN, or rotate it from your account"}
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return &permanentError{msg: fmt.Sprintf("the server rejected the batch: %s", string(detail))}
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("rate limited by the server, backing off")
	default:
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(detail))
	}
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
