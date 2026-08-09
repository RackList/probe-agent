package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMeasureAnchorsTimesEveryReachableAnchor(t *testing.T) {
	first := listeningEndpoint(t)
	second := listeningEndpoint(t)

	observations := measureAnchors(context.Background(), []anchor{
		{ID: 1, Endpoint: first},
		{ID: 2, Endpoint: second},
	})

	if len(observations) != 2 {
		t.Fatalf("got %d observations, want 2: %+v", len(observations), observations)
	}
	for _, o := range observations {
		if o.RTTMs <= 0 {
			t.Errorf("anchor %d reported a round trip of %v, which did not happen", o.AnchorID, o.RTTMs)
		}
	}
}

// "I could not reach it" is not a distance. Reporting an unreachable anchor as
// a very large round trip would push the estimate away from where the probe
// actually is, so it is left out entirely.
func TestMeasureAnchorsSkipsUnreachableAnchors(t *testing.T) {
	reachable := listeningEndpoint(t)

	observations := measureAnchors(context.Background(), []anchor{
		{ID: 1, Endpoint: reachable},
		{ID: 2, Endpoint: closedEndpoint(t)},
		{ID: 3, Endpoint: ""},
	})

	if len(observations) != 1 || observations[0].AnchorID != 1 {
		t.Fatalf("only the reachable anchor should be reported, got %+v", observations)
	}
}

func TestMeasureAnchorsStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if observations := measureAnchors(ctx, []anchor{{ID: 1, Endpoint: listeningEndpoint(t)}}); len(observations) != 0 {
		t.Fatalf("a cancelled run must not report anything, got %+v", observations)
	}
}

func TestSubmitCalibrationSendsTheObservationsAndTheBearer(t *testing.T) {
	var gotAuth string
	var payload struct {
		Observations []observation `json:"observations"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"accepted_observations": 2,
				"accuracy_km":           240.5,
				"confidence":            "high",
			},
		})
	}))
	defer server.Close()

	api := newAPIClient(&config{Token: "pb_abcdef0123456789", API: server.URL, Insecure: true, SubmitTimeout: 5 * time.Second})

	result, err := api.submitCalibration(context.Background(), []observation{
		{AnchorID: 1, RTTMs: 2.5},
		{AnchorID: 2, RTTMs: 18.25},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotAuth != "Bearer pb_abcdef0123456789" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
	if len(payload.Observations) != 2 || payload.Observations[1].RTTMs != 18.25 {
		t.Fatalf("observations not sent as measured: %+v", payload.Observations)
	}
	if result.Data.AcceptedObservations != 2 || result.Data.Confidence != "high" {
		t.Fatalf("verdict not read back: %+v", result.Data)
	}
}

func TestConfigBuildsTheCalibrationUrl(t *testing.T) {
	withEnv(t, map[string]string{
		"PROBE_TOKEN": "pb_abcdef0123456789",
		"PROBE_API":   "https://racklist.eu/api/v1/probe/",
	})

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := cfg.calibrationURL(), "https://racklist.eu/api/v1/probe/calibration"; got != want {
		t.Errorf("calibration URL = %q, want %q", got, want)
	}
}

// A port that accepts connections, which is all an anchor has to offer: the
// agent times the TCP handshake, not an application.
func listeningEndpoint(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	return listener.Addr().String()
}

func closedEndpoint(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	return address
}
