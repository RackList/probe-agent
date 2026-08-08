package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestLoadConfigRequiresAToken(t *testing.T) {
	withEnv(t, map[string]string{"PROBE_API": "https://racklist.eu/api/v1/probe"})

	if _, err := loadConfig(); err == nil {
		t.Fatal("an agent without a token must refuse to start")
	}
}

func TestLoadConfigRejectsATokenThatIsNotAProbeToken(t *testing.T) {
	withEnv(t, map[string]string{
		"PROBE_TOKEN": "rk_something_else",
		"PROBE_API":   "https://racklist.eu/api/v1/probe",
	})

	if _, err := loadConfig(); err == nil {
		t.Fatal("a token without the pb_ prefix must be refused up front")
	}
}

// The token is a bearer secret. Sending it over plain HTTP would hand it to
// anyone on the path, so the agent refuses rather than warns.
func TestLoadConfigRefusesPlainHttpUnlessExplicitlyOverridden(t *testing.T) {
	withEnv(t, map[string]string{
		"PROBE_TOKEN": "pb_abcdef0123456789",
		"PROBE_API":   "http://racklist.dev.localhost/api/v1/probe",
	})

	if _, err := loadConfig(); err == nil {
		t.Fatal("plain http must be refused by default")
	}

	t.Setenv("PROBE_INSECURE", "true")
	if _, err := loadConfig(); err != nil {
		t.Fatalf("the development escape hatch must work: %v", err)
	}
}

func TestLoadConfigBuildsTheEndpointUrls(t *testing.T) {
	withEnv(t, map[string]string{
		"PROBE_TOKEN": "pb_abcdef0123456789",
		"PROBE_API":   "https://racklist.eu/api/v1/probe/",
	})

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := cfg.configURL(), "https://racklist.eu/api/v1/probe/config"; got != want {
		t.Errorf("config URL = %q, want %q", got, want)
	}
	if got, want := cfg.measurementsURL(), "https://racklist.eu/api/v1/probe/measurements"; got != want {
		t.Errorf("measurements URL = %q, want %q", got, want)
	}
}

func TestEnvDurationAcceptsBareSeconds(t *testing.T) {
	t.Setenv("PROBE_TIMEOUT", "45")

	d, err := envDuration("PROBE_TIMEOUT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 45*time.Second {
		t.Errorf("got %v, want 45s", d)
	}
}

func TestEnvDurationRejectsNonPositiveValues(t *testing.T) {
	t.Setenv("PROBE_TIMEOUT", "0")

	if _, err := envDuration("PROBE_TIMEOUT"); err == nil {
		t.Fatal("a zero timeout must be refused")
	}
}

func TestFetchConfigReadsThePoolAndSendsTheBearer(t *testing.T) {
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"probe":                        map[string]any{"id": 12, "label": "VPS Francfort", "status": "probation"},
				"measurement_interval_seconds": 300,
				"config_refresh_seconds":       3600,
				"max_batch_size":               100,
				"targets": []map[string]any{
					{"id": 4, "url": "https://example.com", "name": "Example"},
				},
			},
		})
	}))
	defer server.Close()

	api := newAPIClient(&config{Token: "pb_abcdef0123456789", API: server.URL, Insecure: true, SubmitTimeout: 5 * time.Second})

	serverCfg, err := api.fetchConfig(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotAuth != "Bearer pb_abcdef0123456789" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if len(serverCfg.Targets) != 1 || serverCfg.Targets[0].ID != 4 {
		t.Fatalf("pool not read back: %+v", serverCfg.Targets)
	}
	if serverCfg.interval() != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", serverCfg.interval())
	}
}

// The cadence is a network-wide parameter. An agent that silently fell back to
// zero on a malformed response would hammer the API.
func TestServerConfigFallsBackToSaneDefaults(t *testing.T) {
	var empty serverConfig

	if empty.interval() <= 0 {
		t.Error("interval must never be zero")
	}
	if empty.refreshInterval() <= 0 {
		t.Error("refresh interval must never be zero")
	}
	if empty.batchSize() <= 0 {
		t.Error("batch size must never be zero")
	}
}

func TestSubmitPostsTheBatch(t *testing.T) {
	var received struct {
		Measurements []sample `json:"measurements"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"stored": 2, "duplicates": 0},
		})
	}))
	defer server.Close()

	api := newAPIClient(&config{Token: "pb_abcdef0123456789", API: server.URL, Insecure: true, SubmitTimeout: 5 * time.Second})

	batch := []sample{
		{TargetID: 4, Timestamp: time.Now().UTC().Format(time.RFC3339), LatencyMs: 24.5, UptimeStatus: true},
		{TargetID: 5, Timestamp: time.Now().UTC().Format(time.RFC3339), LatencyMs: 88.1, UptimeStatus: false},
	}

	result, err := api.submit(context.Background(), batch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Data.Stored != 2 {
		t.Errorf("stored = %d, want 2", result.Data.Stored)
	}
	if len(received.Measurements) != 2 || received.Measurements[1].TargetID != 5 {
		t.Fatalf("batch not received as sent: %+v", received.Measurements)
	}
}

// A rejected token is rejected identically on the next try. Retrying would only
// burn the rate limit budget and hide the real cause from the operator.
func TestSubmitDoesNotRetryAnAuthenticationFailure(t *testing.T) {
	attempts := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	api := newAPIClient(&config{Token: "pb_abcdef0123456789", API: server.URL, Insecure: true, SubmitTimeout: 5 * time.Second})
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	err := submitWithBackoff(context.Background(), api, []sample{{TargetID: 1, LatencyMs: 1}}, logger)
	if err == nil {
		t.Fatal("a 401 must surface as an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on a permanent failure)", attempts)
	}
}

func TestSubmitDoesNotRetryARejectedPayload(t *testing.T) {
	attempts := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer server.Close()

	api := newAPIClient(&config{Token: "pb_abcdef0123456789", API: server.URL, Insecure: true, SubmitTimeout: 5 * time.Second})
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := submitWithBackoff(context.Background(), api, []sample{{TargetID: 1, LatencyMs: 1}}, logger); err == nil {
		t.Fatal("a 422 must surface as an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

func TestMeasureReportsAnUnreachableTargetInsteadOfDroppingIt(t *testing.T) {
	cfg := &config{ProbeTimeout: 500 * time.Millisecond}

	// Reserved for documentation, guaranteed not to answer (RFC 5737).
	samples := measureAll(context.Background(), cfg, []target{
		{ID: 9, URL: "https://192.0.2.1:9/"},
	})

	if len(samples) != 1 {
		t.Fatalf("an unreachable target must still produce a sample, got %d", len(samples))
	}
	if samples[0].UptimeStatus {
		t.Error("an unreachable target must be reported as down")
	}
	if samples[0].LatencyMs <= 0 {
		t.Error("latency must stay strictly positive: the server refuses zero")
	}
}

func TestMeasureReportsAReachableTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	samples := measureAll(context.Background(), &config{ProbeTimeout: 5 * time.Second, Insecure: true}, []target{
		{ID: 3, URL: server.URL},
	})

	if len(samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(samples))
	}
	if !samples[0].UptimeStatus {
		t.Error("a 200 must be reported as up")
	}
	if samples[0].TargetID != 3 {
		t.Errorf("target id = %d, want 3", samples[0].TargetID)
	}
}

// An authentication wall still proves the service answered: reporting it as
// down would turn a protected endpoint into a permanent fake outage.
func TestMeasureTreatsAnAuthWallAsUp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	samples := measureAll(context.Background(), &config{ProbeTimeout: 5 * time.Second}, []target{{ID: 1, URL: server.URL}})

	if !samples[0].UptimeStatus {
		t.Error("a 401 from the target must count as reachable")
	}
}

func TestMeasureTreatsAServerErrorAsDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	samples := measureAll(context.Background(), &config{ProbeTimeout: 5 * time.Second}, []target{{ID: 1, URL: server.URL}})

	if samples[0].UptimeStatus {
		t.Error("a 500 must count as down")
	}
}

func TestChunkRespectsTheServerBatchSize(t *testing.T) {
	samples := make([]sample, 250)
	batches := chunk(samples, 100)

	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3", len(batches))
	}
	if len(batches[2]) != 50 {
		t.Errorf("last batch has %d samples, want 50", len(batches[2]))
	}
}

func withEnv(t *testing.T, env map[string]string) {
	t.Helper()

	for _, key := range []string{"PROBE_TOKEN", "PROBE_API", "PROBE_INSECURE", "PROBE_TIMEOUT", "PROBE_SUBMIT_TIMEOUT"} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
}
