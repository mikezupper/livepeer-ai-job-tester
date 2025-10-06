package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	status "livepeer-job-tester/internal/gateway/status"
)

// Client wraps ffmpeg/ffprobe process management so other packages remain decoupled from command details.
type Client interface {
	RunStream(ctx context.Context, ingestURL, playbackURL, testVideoPath string, opts StreamOptions) (*Metrics, error)
}

// StreamStatusClient defines the behaviour needed to wait for Gateway readiness.
type StreamStatusClient interface {
	WaitReady(ctx context.Context, endpoint string, timeout, interval time.Duration) (time.Duration, error)
}

// StreamOptions tunes the behaviour of the ffmpeg client.
type StreamOptions struct {
	StatusEndpoint     string
	StatusPollInterval time.Duration
	StatusPollTimeout  time.Duration
	TestDuration       time.Duration
	MetricRetryDelay   time.Duration
	MaxMetricAttempts  int
}

const (
	defaultStatusPollInterval = 1 * time.Second
	defaultStatusPollTimeout  = 30 * time.Second
	defaultTestDuration       = 30 * time.Second
	defaultMetricRetryDelay   = 200 * time.Millisecond
	defaultMaxMetricAttempts  = 20
)

var errNoFrames = errors.New("live probe produced no frames")

type client struct {
	logger       *slog.Logger
	statusClient StreamStatusClient
}

// NewClient constructs a Client instance using the provided logger and status client for structured output.
func NewClient(logger *slog.Logger, statusClient StreamStatusClient) Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &client{logger: logger, statusClient: statusClient}
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.StatusPollInterval <= 0 {
		o.StatusPollInterval = defaultStatusPollInterval
	}
	if o.StatusPollTimeout <= 0 {
		o.StatusPollTimeout = defaultStatusPollTimeout
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

type readinessResult struct {
	duration time.Duration
	err      error
}

func (c *client) waitForReadiness(ctx context.Context, ffmpegErrCh <-chan error, opts StreamOptions, log *slog.Logger) (time.Duration, error) {
	if c.statusClient == nil || opts.StatusEndpoint == "" {
		return 0, nil
	}

	readyCtx, readyCancel := context.WithCancel(ctx)
	readyCh := make(chan readinessResult, 1)
	go func() {
		dur, err := c.statusClient.WaitReady(readyCtx, opts.StatusEndpoint, opts.StatusPollTimeout, opts.StatusPollInterval)
		readyCh <- readinessResult{duration: dur, err: err}
	}()

	select {
	case res := <-readyCh:
		if res.err != nil {
			if errors.Is(res.err, status.ErrTimeout) {
				return 0, fmt.Errorf("gateway did not report stream ready within %s", opts.StatusPollTimeout)
			}
			return 0, res.err
		}
		log.InfoContext(ctx, "gateway reported live stream ready", slog.Duration("wait", res.duration))
		readyCancel()
		return res.duration, nil
	case ffmpegErr := <-ffmpegErrCh:
		readyCancel()
		go func() { <-readyCh }()
		if ffmpegErr == nil {
			ffmpegErr = errors.New("ffmpeg exited before readiness completed")
		}
		return 0, fmt.Errorf("ffmpeg exited before stream became ready: %w", ffmpegErr)
	case <-ctx.Done():
		readyCancel()
		res := <-readyCh
		if res.err != nil && !errors.Is(res.err, context.Canceled) {
			log.DebugContext(ctx, "readiness watcher returned", slog.Any("error", res.err))
		}
		return 0, ctx.Err()
	}
}

func (c *client) collectMetricsWithRetry(ctx context.Context, push *streamPush, ffmpegErrCh <-chan error, playbackURL string, opts StreamOptions, log *slog.Logger) (*Metrics, error) {
	var (
		metrics    *Metrics
		metricsErr error
	)

	for attempt := 1; attempt <= opts.MaxMetricAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case exitErr := <-ffmpegErrCh:
			if exitErr != nil && !errors.Is(exitErr, context.Canceled) {
				return nil, fmt.Errorf("ffmpeg exited before metrics were collected: %w", exitErr)
			}
			return nil, fmt.Errorf("ffmpeg exited before metrics were collected")
		default:
		}

		attemptCtx, attemptCancel := context.WithCancel(context.Background())
		metrics, metricsErr = c.collectLiveVideoMetrics(attemptCtx, playbackURL, opts.TestDuration, push.StartTime())
		attemptCancel()

		if metricsErr == nil {
			return metrics, nil
		}

		if attempt == opts.MaxMetricAttempts {
			break
		}

		log.WarnContext(ctx, "metrics probe attempt failed",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", opts.MaxMetricAttempts),
			slog.Any("error", metricsErr))

		// Retry regardless of the error, up to the max attempts
		if opts.MetricRetryDelay > 0 {
			log.InfoContext(ctx, "retrying metrics probe", slog.Duration("retry_delay", opts.MetricRetryDelay))
			time.Sleep(opts.MetricRetryDelay)
		}
	}

	return metrics, metricsErr
}

// RunStream pushes a test video to the ingest URL and probes playback to produce delivery metrics.
func (c *client) RunStream(ctx context.Context, ingestURL, playbackURL, testVideoPath string, opts StreamOptions) (*Metrics, error) {
	opts = opts.withDefaults()

	if _, err := os.Stat(testVideoPath); err != nil {
		return nil, fmt.Errorf("unable to access test video file (%s): %w", testVideoPath, err)
	}

	log := c.log()
	log.InfoContext(ctx, "starting live video stream", slog.String("ingest", ingestURL), slog.String("playback", playbackURL))

	push, err := startStreamPush(testVideoPath, ingestURL, log)
	if err != nil {
		log.ErrorContext(ctx, "failed to start ffmpeg", slog.Any("error", err))
		return nil, err
	}
	defer push.Cancel()

	ffmpegErrCh := push.ErrChan()
	readyDuration, err := c.waitForReadiness(ctx, ffmpegErrCh, opts, log)
	if err != nil {
		push.Cancel()
		return nil, err
	}

	metrics, metricsErr := c.collectMetricsWithRetry(ctx, push, ffmpegErrCh, playbackURL, opts, log)
	if metricsErr != nil {
		push.Cancel()
		select {
		case exitErr := <-ffmpegErrCh:
			if exitErr != nil && !errors.Is(exitErr, context.Canceled) {
				log.WarnContext(ctx, "ffmpeg exited during metrics collection", slog.Any("error", exitErr))
			}
		default:
		}
		return nil, metricsErr
	}

	push.Cancel()
	exitErr := <-ffmpegErrCh
	metrics.SetGatewayReadySeconds(readyDuration.Seconds())

	if exitErr != nil {
		if _, ok := exitErr.(*exec.ExitError); ok {
			log.DebugContext(ctx, "ffmpeg exited after cancellation", slog.Any("error", exitErr))
		} else if !errors.Is(exitErr, context.Canceled) {
			log.WarnContext(ctx, "ffmpeg exited with error", slog.Any("error", exitErr))
		}
	}

	log.InfoContext(ctx, "live video stream completed",
		slog.Int("total_frames", metrics.totalFrames),
		slog.Float64("average_fps", metrics.averageFPS),
		slog.Float64("average_latency", metrics.averageLatency),
		slog.Float64("duration_seconds", metrics.durationSeconds),
		slog.Float64("initial_latency_seconds", metrics.initialLatency),
		slog.Float64("gateway_ready_seconds", readyDuration.Seconds()),
	)

	return metrics, nil
}

func (c *client) collectLiveVideoMetrics(ctx context.Context, playbackURL string, duration time.Duration, ingestStart time.Time) (*Metrics, error) {
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	timer := time.AfterFunc(duration, cancelProbe)
	defer timer.Stop()

	go func() {
		<-ctx.Done()
		cancelProbe()
	}()

	args := buildFFProbeArgs(playbackURL)

	c.log().DebugContext(ctx, "ffprobe command", slog.String("args", strings.Join(args, " ")))

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
	pipeProcessOutput(ctx, stderr, c.logger, "ffprobe")

	if err := cmd.Start(); err != nil {
		cancelProbe()
		return nil, err
	}
	defer cancelProbe()

	metrics := newMetrics(ingestStart)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	for scanner.Scan() {
		line := scanner.Text()
		c.log().DebugContext(ctx, "raw ffprobe output", slog.String("line", line))

		select {
		case <-ctx.Done():
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
		cancelProbe()
		<-waitCh
		return nil, err
	}

	waitErr := <-waitCh
	if waitErr != nil && errors.Is(waitErr, context.Canceled) && metrics.totalFrames > 0 {
		waitErr = nil
	}
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) && metrics.totalFrames == 0 {
		return nil, waitErr
	}

	if metrics.totalFrames == 0 {
		return nil, errNoFrames
	}

	metrics.finalize(duration)
	return metrics, nil
}

func pipeProcessOutput(ctx context.Context, reader io.ReadCloser, logger *slog.Logger, prefix string) {
	go func() {
		defer reader.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		for scanner.Scan() {
			logger.DebugContext(ctx, "process output", slog.String("prefix", prefix), slog.String("line", scanner.Text()))
		}
	}()
}

func buildFFProbeArgs(playbackURL string) []string {
	return []string{
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
}

func (c *client) log() *slog.Logger {
	pc, _, _, ok := runtime.Caller(1)
	if !ok {
		return c.logger
	}
	fn := runtime.FuncForPC(pc)
	name := "unknown"
	if fn != nil {
		full := fn.Name()
		if idx := strings.LastIndex(full, "."); idx >= 0 && idx+1 < len(full) {
			name = full[idx+1:]
		} else {
			name = full
		}
	}
	return c.logger.With(slog.String("fn", name))
}
