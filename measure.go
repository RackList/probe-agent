package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// One observation of one target, in the shape the server accepts.
type sample struct {
	TargetID        int64   `json:"target_id"`
	Timestamp       string  `json:"timestamp"`
	LatencyMs       float64 `json:"latency_ms"`
	UptimeStatus    bool    `json:"uptime_status"`
	DNSResolutionMs float64 `json:"dns_resolution_ms,omitempty"`
	TTFBMs          float64 `json:"ttfb_ms,omitempty"`
	SSLExpiryDays   *int    `json:"ssl_expiry_days,omitempty"`
}

// How many targets are measured at once. Enough to keep a round short, low
// enough that the agent does not itself become the bottleneck it is measuring:
// a saturated uplink would inflate every latency in the round.
const maxConcurrentProbes = 4

// measureAll probes every target of the pool and returns one sample each.
// A target that fails still produces a sample: "unreachable from here" is a
// measurement, and dropping it would silently turn an outage into missing data.
func measureAll(ctx context.Context, cfg *config, targets []target) []sample {
	samples := make([]sample, len(targets))
	sem := make(chan struct{}, maxConcurrentProbes)

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			samples[i] = measureOne(ctx, cfg, t)
		}(i, t)
	}
	wg.Wait()

	// A cancelled round leaves zero-valued entries behind; the server would
	// reject them anyway, so drop them here rather than send a bad batch.
	out := make([]sample, 0, len(samples))
	for _, s := range samples {
		if s.TargetID != 0 {
			out = append(out, s)
		}
	}

	return out
}

func measureOne(ctx context.Context, cfg *config, t target) sample {
	probeCtx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout)
	defer cancel()

	var dnsStart, dnsEnd, firstByte time.Time

	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { dnsEnd = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}

	s := sample{
		TargetID:  t.ID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(probeCtx, trace),
		http.MethodGet, t.URL, nil,
	)
	if err != nil {
		s.LatencyMs = floorLatency(0)
		return s
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure}, //nolint:gosec // guarded by config validation
		},
		Timeout: cfg.ProbeTimeout,
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	s.LatencyMs = floorLatency(roundMs(elapsed))

	if err != nil {
		s.UptimeStatus = false
		return s
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	// A redirect or an authentication wall still proves the service answered.
	// Only 4xx beyond auth and 5xx count as down.
	s.UptimeStatus = resp.StatusCode < 400 || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden

	if !dnsStart.IsZero() && !dnsEnd.IsZero() {
		s.DNSResolutionMs = roundMs(dnsEnd.Sub(dnsStart))
	}
	if !firstByte.IsZero() {
		s.TTFBMs = roundMs(firstByte.Sub(start))
	}
	if days, ok := sslExpiryDays(t.URL, cfg.Insecure); ok {
		s.SSLExpiryDays = &days
	}

	return s
}

// The server refuses a latency of zero, and rightly so: a round trip that took
// no measurable time did not happen. Report the smallest honest value instead
// of a zero the server would throw away along with the rest of the batch.
func floorLatency(ms float64) float64 {
	if ms <= 0 {
		return 0.001
	}
	return ms
}

func sslExpiryDays(rawURL string, insecure bool) (int, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return 0, false
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: insecure, //nolint:gosec // guarded by config validation
	})
	if err != nil {
		return 0, false
	}
	defer conn.Close()

	var leaf *x509.Certificate
	for _, c := range conn.ConnectionState().PeerCertificates {
		if !c.IsCA {
			leaf = c
			break
		}
	}
	if leaf == nil {
		return 0, false
	}

	return int(time.Until(leaf.NotAfter).Hours() / 24), true
}

func roundMs(d time.Duration) float64 {
	return math.Round(float64(d.Nanoseconds())/1e6*1000) / 1000
}
