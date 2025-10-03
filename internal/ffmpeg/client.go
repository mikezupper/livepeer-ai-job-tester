package ffmpeg

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client wraps ffmpeg/ffprobe process management so other packages remain decoupled from command details.
type Client interface {
	RunStream(ctx context.Context, ingestURL, playbackURL, testVideoPath string, opts StreamOptions) (*Metrics, error)
}

// StreamOptions tunes the behaviour of the ffmpeg client.
type StreamOptions struct {
	GracePeriod       time.Duration
	TestDuration      time.Duration
	MetricRetryDelay  time.Duration
	MaxMetricAttempts int
}

const (
	defaultGracePeriod       = 5 * time.Second
	defaultTestDuration      = 30 * time.Second
	defaultMetricRetryDelay  = 5 * time.Second
	defaultMaxMetricAttempts = 3
)

type client struct {
	logger *slog.Logger
}

// NewClient constructs a Client instance using the provided logger for structured output.
func NewClient(logger *slog.Logger) Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &client{logger: logger}
}

// RunStream pushes a test video to the ingest URL and probes playback to produce delivery metrics.
func (c *client) RunStream(ctx context.Context, ingestURL, playbackURL, testVideoPath string, opts StreamOptions) (*Metrics, error) {
	opts = opts.withDefaults()

	if _, err := os.Stat(testVideoPath); err != nil {
		return nil, fmt.Errorf("unable to access test video file (%s): %w", testVideoPath, err)
	}

	c.logger.InfoContext(ctx, "starting live video stream", slog.String("ingest", ingestURL), slog.String("playback", playbackURL))

	// Independent context for ffmpeg (live video push)
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()

	cmd, err := c.startLiveVideoPush(streamCtx, testVideoPath, ingestURL)
	if err != nil {
		c.logger.ErrorContext(ctx, "failed to start ffmpeg", slog.Any("error", err))
		return nil, err
	}

	ffmpegErrCh := make(chan error, 1)
	go func() {
		ffmpegErrCh <- cmd.Wait()
	}()

	ffmpegDone := false
	var ffmpegErr error

	// Wait for the grace period to allow the stream to stabilize
	select {
	case ffmpegErr = <-ffmpegErrCh:
		ffmpegDone = true
		c.logger.WarnContext(ctx, "ffmpeg exited before metrics collection", slog.Any("error", ffmpegErr))
		return nil, fmt.Errorf("ffmpeg exited before probe start: %w", ffmpegErr)
	case <-time.After(opts.GracePeriod):
	}

	// Independent context for metrics probe
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	defer metricsCancel()

	streamStart := time.Now()

	var metrics *Metrics
	var lastErr error

	// Use a WaitGroup to ensure both ffmpeg and metrics collection complete
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		for attempt := 1; attempt <= opts.MaxMetricAttempts; attempt++ {
			attemptCtx, attemptCancel := context.WithCancel(metricsCtx)

			metricsCh := make(chan *Metrics, 1)
			errCh := make(chan error, 1)

			c.logger.InfoContext(ctx, "collecting playback metrics", slog.Int("attempt", attempt), slog.Int("max_attempts", opts.MaxMetricAttempts))

			go func() {
				m, err := c.collectLiveVideoMetrics(attemptCtx, playbackURL, opts.TestDuration, streamStart)
				if err != nil {
					errCh <- err
					return
				}
				metricsCh <- m
			}()

			select {
			case metrics = <-metricsCh:
				// Metrics collected successfully
				attemptCancel()
				lastErr = nil
				return
			case err := <-errCh:
				// Metrics collection failed, retry if possible
				attemptCancel()
				lastErr = err
				c.logger.WarnContext(ctx, "metrics collection failed", slog.Int("attempt", attempt), slog.Any("error", err))
				if attempt < opts.MaxMetricAttempts {
					time.Sleep(opts.MetricRetryDelay)
					continue
				}
			}

			break
		}
	}()

	// Wait for either ffmpeg to finish or metrics collection to complete
	wg.Wait()

	// Finalize metrics and stop the stream
	c.logger.InfoContext(ctx, "finalizing stream and metrics collection")
	metricsCancel()
	streamCancel()

	if !ffmpegDone {
		ffmpegErr = <-ffmpegErrCh
		ffmpegDone = true
	}

	if lastErr != nil {
		c.logger.ErrorContext(ctx, "failed to collect playback metrics", slog.Any("error", lastErr))
		if ffmpegErr != nil && !errors.Is(ffmpegErr, context.Canceled) {
			return nil, fmt.Errorf("metrics collection failed: %w (ffmpeg err: %v)", lastErr, ffmpegErr)
		}
		return nil, lastErr
	}

	if ffmpegErr != nil && !errors.Is(ffmpegErr, context.Canceled) {
		c.logger.WarnContext(ctx, "ffmpeg exited with error", slog.Any("error", ffmpegErr))
	}

	metrics.durationSeconds = time.Since(streamStart).Seconds()

	c.logger.InfoContext(ctx, "live video stream completed",
		slog.Int("total_frames", metrics.totalFrames),
		slog.Float64("average_fps", metrics.averageFPS),
		slog.Float64("average_latency", metrics.averageLatency),
		slog.Float64("duration_seconds", metrics.durationSeconds),
	)

	return metrics, nil
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.GracePeriod <= 0 {
		o.GracePeriod = defaultGracePeriod
	}
	if o.TestDuration <= 0 {
		o.TestDuration = defaultTestDuration
	}
	if o.MetricRetryDelay <= 0 {
		o.MetricRetryDelay = defaultMetricRetryDelay
	}
	if o.MaxMetricAttempts <= 0 {
		o.MaxMetricAttempts = defaultMaxMetricAttempts
	}
	return o
}

func (c *client) startLiveVideoPush(ctx context.Context, videoPath, ingestURL string) (*exec.Cmd, error) {
	args := []string{
		"-re",
		"-stream_loop", "-1", // Loop input indefinitely to ensure continuous streaming for metrics collection
		"-loglevel", "error",
		"-i", videoPath,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-an",
		"-r", "30", // Output frame rate
		"-g", "60", // GOP size (keyframe every 2 seconds)
		"-force_key_frames", "expr:gte(t,n_forced*2)", // Force keyframes every 2 seconds
		"-vsync", "cfr", // Ensure constant frame rate
		"-f", "flv",
		ingestURL,
	}

	c.logger.DebugContext(ctx, "starting ffmpeg", slog.Any("args", args))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = io.Discard

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	c.pipeProcessOutput(ctx, stderr, "ffmpeg")

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}

func (c *client) collectLiveVideoMetrics(ctx context.Context, playbackURL string, duration time.Duration, streamStart time.Time) (*Metrics, error) {
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	timer := time.AfterFunc(duration, cancelProbe)
	defer timer.Stop()

	go func() {
		<-ctx.Done()
		cancelProbe()
	}()

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-select_streams", "v:0",
		"-show_frames",
		"-show_entries", "frame=pkt_pts_time,best_effort_timestamp_time",
		"-of", "compact=p=0:nk=1",
		"-probesize", "32M",
		"-analyzeduration", "5M",
		"-read_intervals", "%+30",
		playbackURL,
	}

	c.logger.DebugContext(ctx, "ffprobe command", slog.String("args", strings.Join(args, " ")))

	cmd := exec.CommandContext(probeCtx, "ffprobe", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancelProbe()
		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancelProbe()
		return nil, err
	}
	c.pipeProcessOutput(ctx, stderr, "ffprobe")

	if err := cmd.Start(); err != nil {
		cancelProbe()
		return nil, err
	}

	metrics := newMetrics()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	for scanner.Scan() {
		line := scanner.Text()
		c.logger.DebugContext(ctx, "raw ffprobe output", slog.String("line", line))

		select {
		case <-ctx.Done():
			cancelProbe()
			<-waitCh
			return nil, ctx.Err()
		default:
		}

		pts, ok := parseFramePTS(ctx, c.logger, line)
		if !ok {
			continue
		}

		arrival := time.Since(streamStart).Seconds()
		metrics.addFrame(ctx, c.logger, pts, arrival)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		cancelProbe()
		<-waitCh
		return nil, err
	}

	waitErr := <-waitCh
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) && metrics.totalFrames == 0 {
		return nil, waitErr
	}

	metrics.finalize(ctx, c.logger, duration)

	return metrics, nil
}

func (c *client) pipeProcessOutput(ctx context.Context, reader io.ReadCloser, prefix string) {
	go func() {
		defer reader.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		for scanner.Scan() {
			c.logger.DebugContext(ctx, "process output", slog.String("prefix", prefix), slog.String("line", scanner.Text()))
		}
	}()
}

// Metrics captures summary data for a stream playback run.
type Metrics struct {
	totalFrames     int
	totalLatency    float64
	lastArrival     float64
	averageFPS      float64
	averageLatency  float64
	durationSeconds float64
}

func newMetrics() *Metrics {
	return &Metrics{}
}

func (m *Metrics) addFrame(ctx context.Context, logger *slog.Logger, pts, arrival float64) {
	if arrival < 0 {
		arrival = 0
	}

	latency := arrival - pts
	if latency < 0 {
		latency = 0
	}

	m.totalLatency += latency
	m.totalFrames++
	m.lastArrival = arrival

	// Log frame details
	logger.DebugContext(ctx, "Frame added", slog.Float64("pts", pts), slog.Float64("arrival", arrival), slog.Float64("latency", latency), slog.Int("totalFrames", m.totalFrames))
}

func (m *Metrics) finalize(ctx context.Context, logger *slog.Logger, targetDuration time.Duration) {
	observed := m.lastArrival
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

	// Log final metrics
	logger.DebugContext(ctx, "Finalizing metrics", slog.Float64("observed", observed), slog.Int("totalFrames", m.totalFrames), slog.Float64("averageFPS", m.averageFPS), slog.Float64("averageLatency", m.averageLatency))
}

// TotalFrames returns the number of video frames processed during the probe.
func (m *Metrics) TotalFrames() int {
	return m.totalFrames
}

// AverageFPS returns the average frames per second observed during playback.
func (m *Metrics) AverageFPS() float64 {
	return m.averageFPS
}

// AverageLatency returns the average frame latency in seconds.
func (m *Metrics) AverageLatency() float64 {
	return m.averageLatency
}

// DurationSeconds returns the observed playback duration in seconds.
func (m *Metrics) DurationSeconds() float64 {
	return m.durationSeconds
}

// ToResponsePayload renders a JSON payload string containing computed metrics alongside an optional response.
func (m *Metrics) ToResponsePayload(startResponse string) string {
	payload := map[string]any{
		"total_frames":            m.totalFrames,
		"average_fps":             roundFloat(m.averageFPS, 2),
		"average_latency_seconds": roundFloat(m.averageLatency, 3),
		"duration_seconds":        roundFloat(m.durationSeconds, 2),
	}

	if startResponse != "" {
		var parsed any
		if err := json.Unmarshal([]byte(startResponse), &parsed); err == nil {
			payload["start_response"] = parsed
		} else {
			payload["start_response_raw"] = startResponse
		}
	}

	output, err := json.Marshal(payload)
	if err != nil {
		return startResponse
	}
	return string(output)
}

func parseFramePTS(ctx context.Context, logger *slog.Logger, line string) (float64, bool) {
	if !strings.HasPrefix(line, "frame|") {
		// Log unexpected output
		logger.DebugContext(ctx, "Unexpected ffprobe output", slog.String("line", line))
		return 0, false
	}

	fields := strings.Split(line, "|")
	for _, field := range fields[1:] {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := parts[0]
		value := parts[1]
		if value == "N/A" {
			continue
		}

		switch key {
		case "pkt_pts_time", "best_effort_timestamp_time":
			parsed, err := strconv.ParseFloat(value, 64)
			if err == nil {
				logger.DebugContext(ctx, "Parsed frame PTS", slog.String("key", key), slog.String("value", value), slog.Float64("parsed", parsed))
				return parsed, true
			}
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
