package ffmpeg

import (
	"math"
	"testing"
)

func TestScore(t *testing.T) {
	tests := []struct {
		name                string
		firstFrameSeen      bool
		averageFPS          float64
		targetFPS           float64
		initialLatency      float64
		gatewayReadySeconds float64
		maxInitialLatency   float64
		expectedScore       float64
	}{
		{
			name:                "High FPS, Low Latency",
			firstFrameSeen:      true,
			averageFPS:          30,
			targetFPS:           30,
			initialLatency:      100,
			gatewayReadySeconds: 0,
			maxInitialLatency:   500,
			expectedScore:       0.94,
		},
		{
			name:                "High FPS, High Latency",
			firstFrameSeen:      true,
			averageFPS:          25,
			targetFPS:           30,
			initialLatency:      600,
			gatewayReadySeconds: 0,
			maxInitialLatency:   500,
			expectedScore:       0.68,
		},
		{
			name:                "High FPS, Low Latency after accounting for Gateway Startup Time",
			firstFrameSeen:      true,
			averageFPS:          25,
			targetFPS:           30,
			initialLatency:      600,
			gatewayReadySeconds: 200,
			maxInitialLatency:   500,
			expectedScore:       0.74,
		},
		{
			name:                "Low FPS, High Latency",
			firstFrameSeen:      true,
			averageFPS:          10,
			targetFPS:           30,
			initialLatency:      600,
			gatewayReadySeconds: 0,
			maxInitialLatency:   500,
			expectedScore:       0.39,
		},
		{
			name:                "Low FPS, Low Latency",
			firstFrameSeen:      true,
			averageFPS:          10,
			targetFPS:           30,
			initialLatency:      250,
			gatewayReadySeconds: 0,
			maxInitialLatency:   500,
			expectedScore:       0.54,
		},
		{
			name:                "Moderate FPS, Moderate Latency",
			firstFrameSeen:      true,
			averageFPS:          20,
			targetFPS:           30,
			initialLatency:      250,
			gatewayReadySeconds: 0,
			maxInitialLatency:   500,
			expectedScore:       0.77,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &Metrics{
				averageFPS:          tt.averageFPS,
				initialLatency:      tt.initialLatency,
				firstFrameSeen:      tt.firstFrameSeen,
				gatewayReadySeconds: tt.gatewayReadySeconds,
			}

			score := metrics.Score(tt.targetFPS, tt.maxInitialLatency)
			if math.Abs(score-tt.expectedScore) > 0.01 {
				t.Errorf("expected score %.2f, got %.2f", tt.expectedScore, score)
			}
		})
	}
}

func TestParseFramePTSSupportsShowinfoOutput(t *testing.T) {
	line := "[Parsed_showinfo_0 @ 0x123] n:   1 pts:   3000 pts_time:3.000 pos:1024 fmt:yuv420p"

	got, ok := parseFramePTS(line)
	if !ok {
		t.Fatal("parseFramePTS() did not parse showinfo output")
	}
	if got != 3.0 {
		t.Fatalf("parseFramePTS() = %v, want 3.0", got)
	}
}
