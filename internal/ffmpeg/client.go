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
	defaultMetricRetryDelay  = 200 * time.Millisecond
	defaultMaxMetricAttempts = 20
)

var errNoFrames = errors.New("live probe produced no frames")

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

	// Start ffmpeg to push the test video to the ingest URL
	// Note: we loop the input indefinitely to ensure continuous streaming for metrics collection
	cmd, ingestStart, err := c.startLiveVideoPush(streamCtx, testVideoPath, ingestURL)
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

	// Wait for grace period to allow stream to start and stabilize
	if opts.GracePeriod > 0 {
		select {
		case ffmpegErr = <-ffmpegErrCh:
			ffmpegDone = true
			c.logger.WarnContext(ctx, "ffmpeg exited during grace period", slog.Any("error", ffmpegErr))
			return nil, fmt.Errorf("ffmpeg exited before probe start: %w", ffmpegErr)
		case <-time.After(opts.GracePeriod):
			c.logger.InfoContext(ctx, "grace period to allow stream to start and stabilize elapsed, starting metrics probe")
		}
	}

	// Independent context for metrics probe
	var metrics *Metrics
	var metricsErr error

	var attempt int
	for attempt = 1; attempt <= opts.MaxMetricAttempts; attempt++ {
		if ctx.Err() != nil {
			c.logger.WarnContext(ctx, "context cancelled metrics probe after %d attempts", slog.Int("attempts", attempt-1), slog.Any("error", ctx.Err()))
			streamCancel()
			if !ffmpegDone {
				ffmpegErr = <-ffmpegErrCh
			}
			return nil, ctx.Err()
		}

		attemptCtx, attemptCancel := context.WithCancel(context.Background())
		metrics, metricsErr = c.collectLiveVideoMetrics(attemptCtx, playbackURL, opts.TestDuration, ingestStart)
		attemptCancel()

		// the probe succeeded, we're done
		if metricsErr == nil {
			break
		}

		// no more attempts left, we're done
		if attempt == opts.MaxMetricAttempts {
			break
		}

		c.logger.WarnContext(ctx, "metrics probe attempt failed",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", opts.MaxMetricAttempts),
			slog.Any("error", metricsErr))

		// if the error was not "no frames", we're done
		// (likely a fatal ffprobe error)
		if errors.Is(metricsErr, errNoFrames) {
			if opts.MetricRetryDelay > 0 {
				c.logger.InfoContext(ctx, "no frames received, retrying metrics probe", slog.Any("error", metricsErr))
				time.Sleep(opts.MetricRetryDelay)
			}
			continue
		}
	}

	// if we exhausted all attempts, return the last error
	if metricsErr != nil {
		streamCancel()
		if !ffmpegDone {
			ffmpegErr = <-ffmpegErrCh
		}
		if ffmpegErr != nil && !errors.Is(ffmpegErr, context.Canceled) {
			return nil, fmt.Errorf("ffmpeg exited before metrics were collected: %w", ffmpegErr)
		}
		return nil, metricsErr
	}

	// clean up ffmpeg process
	streamCancel()
	cancelledByTester := true
	if !ffmpegDone {
		ffmpegErr = <-ffmpegErrCh
		ffmpegDone = true
	}
	if ffmpegErr != nil {
		if _, ok := ffmpegErr.(*exec.ExitError); ok && cancelledByTester {
			// Ignore our own SIGKILL/SIGTERM (expected when we cancel the context)
		} else if !errors.Is(ffmpegErr, context.Canceled) {
			c.logger.WarnContext(ctx, "ffmpeg exited with error", slog.Any("error", ffmpegErr))
		}
	}

	c.logger.InfoContext(ctx, "live video stream completed",
		slog.Int("total_frames", metrics.totalFrames),
		slog.Float64("average_fps", metrics.averageFPS),
		slog.Float64("average_latency", metrics.averageLatency),
		slog.Float64("duration_seconds", metrics.durationSeconds),
		slog.Float64("initial_latency_seconds", metrics.initialLatency),
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

func (c *client) startLiveVideoPush(ctx context.Context, videoPath, ingestURL string) (*exec.Cmd, time.Time, error) {
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
		return nil, time.Time{}, err
	}
	c.pipeProcessOutput(ctx, stderr, "ffmpeg")

	if err := cmd.Start(); err != nil {
		return nil, time.Time{}, err
	}

	return cmd, time.Now(), nil
}

func (c *client) collectLiveVideoMetrics(ctx context.Context, playbackURL string, duration time.Duration, ingestStart time.Time) (*Metrics, error) {
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	// Ensure the probe is cancelled after the specified test duration has elapsed
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
		"-of", "default=noprint_wrappers=1:nokey=0",
		"-probesize", "32M",
		"-analyzeduration", "5M",
		"-read_intervals", "%+30",
		playbackURL,
	}

	// Prefix the output with a custom string
	// args = append(args, "-prefix", "custom_prefix")

	c.logger.DebugContext(ctx, "ffprobe command", slog.String("args", strings.Join(args, " ")))

	cmd := exec.CommandContext(probeCtx, "ffprobe", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.logger.ErrorContext(ctx, "failed to create ffprobe stdout pipe", slog.Any("error", err))
		cancelProbe()
		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.logger.ErrorContext(ctx, "failed to create ffprobe stderr pipe", slog.Any("error", err))
		cancelProbe()
		return nil, err
	}
	c.pipeProcessOutput(ctx, stderr, "ffprobe")

	if err := cmd.Start(); err != nil {
		c.logger.ErrorContext(ctx, "failed to start ffprobe", slog.Any("error", err))
		cancelProbe()
		return nil, err
	}
	defer cancelProbe()

	metrics := newMetrics(ingestStart)

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
			c.logger.DebugContext(ctx, "ffprobe context cancelled")
			cancelProbe()
			<-waitCh
			return nil, ctx.Err()
		default:
		}

		pts, ok := parseFramePTS(line)
		if !ok {
			continue
		}

		arrivalSinceIngest := time.Since(ingestStart).Seconds()
		metrics.addFrame(pts, arrivalSinceIngest)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		c.logger.ErrorContext(ctx, "error reading ffprobe output", slog.Any("error", err))
		cancelProbe()
		<-waitCh
		return nil, err
	}

	waitErr := <-waitCh
	// if the context was cancelled but we did receive frames, ignore the error
	// (this means the probe ran for the full duration and was cancelled as expected)
	// if no frames were received, return the error (likely ffprobe failed)
	if waitErr != nil && errors.Is(waitErr, context.Canceled) && metrics.totalFrames > 0 {
		c.logger.DebugContext(ctx, "ffprobe exited due to context cancellation after receiving frames")
		waitErr = nil
	}
	// if ffprobe failed for any other reason, return the error
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) && metrics.totalFrames == 0 {
		c.logger.ErrorContext(ctx, "ffprobe exited with error before receiving frames", slog.Any("error", waitErr))
		return nil, waitErr
	}

	// if we didn't receive any frames, return an error
	if metrics.totalFrames == 0 {
		return nil, errNoFrames
	}

	metrics.finalize(duration)

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
	ingestStart     time.Time
	initialLatency  float64
	firstArrival    float64
	firstFrameSeen  bool
	firstPTS        float64
	lastPTS         float64
}

func newMetrics(ingestStart time.Time) *Metrics {
	return &Metrics{
		ingestStart: ingestStart,
	}
}

// addFrame processes the arrival of a video frame and updates metrics related to latency and frame tracking.
//
// Parameters:
// - pts: The presentation timestamp of the frame, representing when the frame is supposed to be displayed.
// - arrivalSinceIngest: The time elapsed since the frame was ingested, representing when the test started pushing the test video asset.
//
// Behavior:
// - If `arrivalSinceIngest` is negative, it is reset to 0 to avoid invalid latency calculations.
// - If this is the first frame being processed, it initializes the first frame metrics:
//   - Marks that the first frame has been seen.
//   - Sets `firstArrival` to the current `arrivalSinceIngest`.
//   - Sets `initialLatency` to the current `arrivalSinceIngest`.
//
// - Calculates the latency for the current frame as the difference between `arrivalSinceIngest` and `pts`.
//   - If the calculated latency is negative, it is reset to 0 to avoid invalid latency values.
//
// - Updates the total latency and frame count metrics:
//   - Adds the calculated latency to `totalLatency`.
//   - Increments the `totalFrames` counter.
//
// - Updates `lastArrival` to the current `arrivalSinceIngest` to track the arrival time of the most recent frame.
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
	// guard against non-monotonic pts; only extend the window
	if pts >= m.lastPTS {
		m.lastPTS = pts
	}
}

// finalize computes and sets the metrics for the observed video playback.
// The goal is to summarize the playback performance by calculating the total observed duration,
// average frames per second (FPS), and average latency per frame based on the collected data.
//
// Parameters:
// - targetDuration: The expected duration of the video playback.
//
// The method performs the following calculations:
//   - Determines the observed duration of the playback based on the difference between the first
//     and last presentation timestamps (PTS). If no valid duration is observed, it falls back to
//     the difference between the first and last frame arrival times, or defaults to the target duration.
//   - Sets the total observed duration in seconds.
//   - Computes the average FPS as the total number of frames divided by the observed duration,
//     provided both values are greater than zero.
//   - Calculates the average latency per frame as the total accumulated latency divided by the
//     total number of frames, if any frames were processed.
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

// InitialLatency returns the time to first frame since ingest began.
func (m *Metrics) InitialLatency() float64 {
	if !m.firstFrameSeen {
		return 0
	}
	return m.initialLatency
}

// Score computes a normalized performance score (0..1) based on target FPS and maximum acceptable initial latency.
// Score calculates a performance score based on the average frames per second (FPS)
// and initial latency metrics. The score is a weighted combination of FPS and latency
// scores, where the FPS score is weighted at 60% and the latency score at 40%.
// The function ensures the score is clamped between 0 and 1.
//
// Parameters:
//   - targetFPS: The desired target frames per second. If greater than 0, it is used
//     to calculate the FPS score.
//   - maxInitialLatency: The maximum acceptable initial latency. If greater than 0
//     and the actual initial latency is available, it is used to calculate the latency score.
//   - graceOffset: A grace period (in seconds) to be subtracted from the initial latency
//     before calculating the latency score.
//
// Returns:
//
//	A float64 value representing the calculated performance score, clamped between 0 and 1.
func (m *Metrics) Score(targetFPS, maxInitialLatency float64, graceOffset int) float64 {
	const (
		fpsWeight     = 0.6
		latencyWeight = 0.4
	)

	fpsScore := 1.0
	if targetFPS > 0 {
		fpsScore = math.Min(1, m.averageFPS/targetFPS)
	}

	latencyScore := 1.0
	initialLatency := m.InitialLatency()

	if maxInitialLatency > 0 && initialLatency > 0 {
		// Ignore the configured grace period that elapses between ingest start and probe start
		effectiveInitialLatency := math.Max(0, initialLatency-float64(graceOffset))
		// calculate score as ratio of max acceptable latency to effective initial latency
		// iow, the Orch has to deliver the first frame within the max acceptable latency to get a score of 1
		// if it takes longer, the score decreases proportionally, but is never negative
		// if it takes less time, the score is 1 (i.e. if effective latency equals max acceptable, score is 1)
		// capped at 1 (i.e. if effective latency is less than max acceptable, score is 1)
		latencyScore = math.Min(1, maxInitialLatency/effectiveInitialLatency)
	}

	return math.Max(0, math.Min(1, fpsWeight*fpsScore+latencyWeight*latencyScore))
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

func parseFramePTS(line string) (float64, bool) {
	line = strings.TrimSpace(line)

	// Handle default format: "pkt_pts_time=59.132000" or "best_effort_timestamp_time=59.132000"
	if after, ok := strings.CutPrefix(line, "pkt_pts_time="); ok {
		value := after
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed, true
		}
	}

	if after, ok := strings.CutPrefix(line, "best_effort_timestamp_time="); ok {
		value := after
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
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
