package main

import (
	"context"
	"net"
	"sort"
	"time"
)

// One round trip towards an anchor, in the shape the server accepts.
type observation struct {
	AnchorID int64   `json:"anchor_id"`
	RTTMs    float64 `json:"rtt_ms"`
}

const (
	// How many times each anchor is measured. The minimum of several attempts
	// is kept: queueing, congestion and retransmits can only ever add to the
	// physical round trip, never subtract from it, so the smallest sample is
	// the closest thing to the distance there is.
	calibrationAttempts = 5

	// A TCP handshake that has not completed by then carries no distance
	// information, only the fact that something is in the way.
	calibrationDialTimeout = 3 * time.Second

	// Breathing room between attempts, so a burst does not measure its own
	// queue.
	calibrationPause = 200 * time.Millisecond
)

// measureAnchors times the TCP handshake towards every anchor the server named.
//
// The handshake rather than an HTTP request on purpose: it is one round trip
// with no application time in it, which is what the server's multilateration
// needs. It also means an anchor needs nothing running beyond a port that
// answers.
//
// An unreachable anchor produces no observation at all rather than a large
// value: "I could not reach it" is not a distance, and reporting it as one
// would push the estimate away from where the probe actually is.
func measureAnchors(ctx context.Context, anchors []anchor) []observation {
	observations := make([]observation, 0, len(anchors))

	for _, a := range anchors {
		if a.Endpoint == "" {
			continue
		}

		if rtt, ok := lowestRTT(ctx, a.Endpoint); ok {
			observations = append(observations, observation{AnchorID: a.ID, RTTMs: rtt})
		}

		select {
		case <-ctx.Done():
			return observations
		default:
		}
	}

	return observations
}

func lowestRTT(ctx context.Context, endpoint string) (float64, bool) {
	samples := make([]float64, 0, calibrationAttempts)
	dialer := &net.Dialer{Timeout: calibrationDialTimeout}

	for attempt := 0; attempt < calibrationAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, false
			case <-time.After(calibrationPause):
			}
		}

		start := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp", endpoint)
		elapsed := time.Since(start)

		if err != nil {
			continue
		}
		_ = conn.Close()

		samples = append(samples, roundMs(elapsed))
	}

	if len(samples) == 0 {
		return 0, false
	}

	sort.Float64s(samples)

	return samples[0], true
}
