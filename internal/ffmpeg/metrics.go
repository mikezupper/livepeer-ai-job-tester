package ffmpeg

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Metrics captures summary data for a stream playback run.
type Metrics struct {
	totalFrames         int
	totalLatency        float64
	lastArrival         float64
	averageFPS          float64
	averageLatency      float64
	durationSeconds     float64
	ingestStart         time.Time
	initialLatency      float64
	firstArrival        float64
	firstFrameSeen      bool
	firstPTS            float64
	lastPTS             float64
	gatewayReadySeconds float64
}

func newMetrics(ingestStart time.Time) *Metrics {
	return &Metrics{ingestStart: ingestStart}
}

func (m *Metrics) addFrame(pts, arrivalSinceIngest float64) {
	if arrivalSinceIngest < 0 {
		arrivalSinceIngest = 0
	}

	if !m.firstFrameSeen {
		m.firstFrameSeen = true
		m.firstArrival = arrivalSinceIngest
		m.initialLatency = arrivalSinceIngest
		m.firstPTS = pts
		m.lastPTS = pts
	}

	latency := arrivalSinceIngest - pts
	if latency < 0 {
		latency = 0
	}

	m.totalLatency += latency
	m.totalFrames++
	m.lastArrival = arrivalSinceIngest
	if pts >= m.lastPTS {
		m.lastPTS = pts
	}
}

func (m *Metrics) finalize(targetDuration time.Duration) {
	var observed float64
	if m.firstFrameSeen {
		observed = m.lastPTS - m.firstPTS
	}

	if observed <= 0 {
		observed = m.lastArrival - m.firstArrival
	}

	if observed <= 0 {
		observed = targetDuration.Seconds()
	}

	m.durationSeconds = observed

	if observed > 0 && m.totalFrames > 0 {
		m.averageFPS = float64(m.totalFrames) / observed
	}

	if m.totalFrames > 0 {
		m.averageLatency = m.totalLatency / float64(m.totalFrames)
	}
}

func (m *Metrics) TotalFrames() int { return m.totalFrames }

func (m *Metrics) AverageFPS() float64 { return m.averageFPS }

func (m *Metrics) AverageLatency() float64 { return m.averageLatency }

func (m *Metrics) DurationSeconds() float64 { return m.durationSeconds }

func (m *Metrics) InitialLatency() float64 {
	if !m.firstFrameSeen {
		return 0
	}
	return m.initialLatency
}

func (m *Metrics) SetGatewayReadySeconds(seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	m.gatewayReadySeconds = seconds
}

func (m *Metrics) GatewayReadySeconds() float64 { return m.gatewayReadySeconds }

func (m *Metrics) Score(targetFPS, maxInitialLatency float64) float64 {
	const (
		fpsWeight     = 0.7 // FPS remains the dominant factor
		latencyWeight = 0.3 // Latency has a lower impact
	)

	// FPS Score: Apply a non-linear penalty based on the distance from targetFPS
	fpsScore := 0.0
	if targetFPS > 0 {
		// Calculate the ratio of averageFPS to targetFPS
		ratio := m.averageFPS / targetFPS

		// Apply a quadratic penalty: closer to 1 gives a higher score, further away penalizes harder
		fpsScore = math.Max(0, 1-math.Pow(1-ratio, 2))
	}

	// Latency Score: Normalize latency to a range of 0 to 1
	latencyScore := 1.0 // Start with the best score for latency
	initialLatency := m.InitialLatency()

	if maxInitialLatency > 0 && initialLatency > 0 {
		effectiveInitialLatency := initialLatency
		if ready := m.GatewayReadySeconds(); ready > 0 {
			effectiveInitialLatency = math.Max(0, initialLatency-ready)
		}
		latencyScore = math.Max(0, 1-(effectiveInitialLatency/maxInitialLatency))
	}

	// Combine FPS and Latency Scores
	// Scale the score to 0–1
	finalScore := fpsWeight*fpsScore + latencyWeight*latencyScore
	return math.Max(0, math.Min(1, finalScore))
}

func parseFramePTS(line string) (float64, bool) {
	line = strings.TrimSpace(line)

	if after, ok := strings.CutPrefix(line, "pkt_pts_time="); ok {
		if parsed, err := strconv.ParseFloat(after, 64); err == nil {
			return parsed, true
		}
	}

	if after, ok := strings.CutPrefix(line, "best_effort_timestamp_time="); ok {
		if parsed, err := strconv.ParseFloat(after, 64); err == nil {
			return parsed, true
		}
	}

	if idx := strings.Index(line, "pts_time:"); idx >= 0 {
		after := line[idx+len("pts_time:"):]
		token := after
		if cut := strings.IndexAny(after, " \t"); cut >= 0 {
			token = after[:cut]
		}
		if parsed, err := strconv.ParseFloat(token, 64); err == nil {
			return parsed, true
		}
	}

	return 0, false
}

func roundFloat(value float64, precision int) float64 {
	if precision < 0 {
		return value
	}
	factor := math.Pow10(precision)
	return math.Round(value*factor) / factor
}
