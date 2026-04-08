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
	"strings"
	"time"

	status "livepeer-job-tester/internal/gateway/status"
)

// Client wraps ffmpeg/ffprobe process management so other packages remain decoupled from command details.
type Client interface {
	RunStream(ctx context.Context, ingestURL, playbackURL, testVideoPath string, opts StreamOptions) (*StreamResult, error)
}

// StreamStatusClient defines the behaviour needed to wait for Gateway readiness.
type StreamStatusClient interface {
	WaitReady(ctx context.Context, endpoint string, timeout, interval time.Duration) (*status.LiveStatusSnapshot, time.Duration, error)
}

// StreamOptions tunes the behaviour of the ffmpeg client.
type StreamOptions struct {
	StatusEndpoint     string
	StatusPollInterval time.Duration
	StatusPollTimeout  time.Duration
	TestDuration       time.Duration
	ManualAttachDelay  time.Duration
	MetricRetryDelay   time.Duration
	MaxMetricAttempts  int
	MaxProbeAttempts   int
}

// StreamResult captures the outcome produced while running a live stream.
type StreamResult struct {
	Metrics *Metrics
}

const (
	defaultStatusPollInterval = 1 * time.Second
	defaultStatusPollTimeout  = 20 * time.Second
	defaultTestDuration       = 30 * time.Second
	defaultMetricRetryDelay   = 200 * time.Millisecond
	defaultMaxMetricAttempts  = 300
	defaultMaxProbeAttempts   = 5
)

var (
	ErrGatewayTimeout = errors.New("ffmpeg: gateway readiness timeout")
	ErrStreamAborted  = errors.New("ffmpeg: stream aborted")
	ErrNoFrames       = errors.New("ffmpeg: no frames received")
	ErrProbeFailed    = errors.New("ffmpeg: probe failed")
)

type client struct {
	logger       *slog.Logger
	probeLogger  *slog.Logger
	statusClient StreamStatusClient
}

// NewClient constructs a Client instance using the provided logger and status client for structured output.
func NewClient(logger *slog.Logger, statusClient StreamStatusClient) (Client, error) {
	if statusClient == nil {
		return nil, errors.New("ffmpeg: status client is required")
	}
	base := logger
	if base == nil {
		base = slog.Default()
	}
	base = base.With(slog.String("component", "ffmpeg"))
	return &client{
		logger:       base,
		probeLogger:  base.With(slog.String("subsystem", "probe")),
		statusClient: statusClient,
	}, nil
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
	if o.MaxProbeAttempts <= 0 {
		o.MaxProbeAttempts = defaultMaxProbeAttempts
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
		_, dur, err := c.statusClient.WaitReady(readyCtx, opts.StatusEndpoint, opts.StatusPollTimeout, opts.StatusPollInterval)
		readyCh <- readinessResult{duration: dur, err: err}
	}()

	select {
	case res := <-readyCh:
		readyCancel()
		if res.err != nil {
			if errors.Is(res.err, status.ErrTimeout) {
				// the live video status endpoint did not report ready in time / assume Orch did not send in segments in time for live video to become healthy
				return 0, fmt.Errorf("%w: %v", ErrGatewayTimeout, res.err)
			}
			if errors.Is(res.err, status.ErrOrchestratorBusy) || errors.Is(res.err, status.ErrNoOrchestratorsAvailable) || errors.Is(res.err, status.ErrGatewayStreamFailed) {
				return 0, fmt.Errorf("%w: %v", ErrStreamAborted, res.err)
			}
			// other errors are likely network related - treat as tester issues
			return 0, fmt.Errorf("gateway readiness failed: %w", res.err)
		}
		log.InfoContext(ctx, "gateway reported live stream ready", slog.Duration("wait", res.duration))
		return res.duration, nil
	case ffmpegErr := <-ffmpegErrCh:
		readyCancel()
		go func() { <-readyCh }()
		if ffmpegErr == nil {
			ffmpegErr = ErrStreamAborted
		}
		// pushing the stream failed or was aborted likely due to the Orchestrator
		return 0, fmt.Errorf("%w: %v", ErrStreamAborted, ffmpegErr)
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
			//pushing the stream failed or was aborted likely due to the Orchestrator
			if exitErr != nil && !errors.Is(exitErr, context.Canceled) {
				return nil, fmt.Errorf("%w: %v", ErrStreamAborted, exitErr)
			}
			// stream was aborted - this occurs when the Orchestrator is unhealthy/capped
			return nil, fmt.Errorf("%w: %v", ErrStreamAborted, exitErr)
		default:
		}

		measurementStart := push.StartTime()
		if opts.ManualAttachDelay > 0 {
			// The manual attach window is for local debugging only. We shift the
			// measurement start by the attach delay so pausing to open ffplay/VLC
			// does not artificially inflate the scored latency metrics.
			measurementStart = measurementStart.Add(opts.ManualAttachDelay)
		}

		attemptCtx, attemptCancel := context.WithCancel(context.Background())
		metrics, metricsErr = c.collectLiveVideoMetrics(attemptCtx, playbackURL, opts.TestDuration, measurementStart)
		attemptCancel()

		// If metrics were collected successfully, return them.
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

		if opts.MetricRetryDelay > 0 {
			time.Sleep(opts.MetricRetryDelay)
		}
	}

	return metrics, metricsErr
}

func (c *client) waitForManualAttachWindow(ctx context.Context, ffmpegErrCh <-chan error, playbackURL string, delay time.Duration, log *slog.Logger) error {
	if delay <= 0 {
		return nil
	}

	log.InfoContext(ctx, "manual attach window open",
		slog.String("playback", playbackURL),
		slog.Duration("delay", delay))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		log.InfoContext(ctx, "manual attach window complete", slog.String("playback", playbackURL))
		return nil
	case err := <-ffmpegErrCh:
		if err == nil {
			err = ErrStreamAborted
		}
		return fmt.Errorf("%w: %v", ErrStreamAborted, err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunStream pushes a test video to the ingest URL and probes playback to produce delivery metrics.
func (c *client) RunStream(ctx context.Context, ingestURL, playbackURL, testVideoPath string, opts StreamOptions) (*StreamResult, error) {
	opts = opts.withDefaults()
	result := &StreamResult{}

	if _, err := os.Stat(testVideoPath); err != nil {
		// Failures accessing the video file is on the tester side
		return nil, fmt.Errorf("unable to access test video file: %w", err)
	}

	log := c.logger.With(slog.String("operation", "RunStream"))
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
		return result, err
	}

	if err := c.waitForManualAttachWindow(ctx, ffmpegErrCh, playbackURL, opts.ManualAttachDelay, log); err != nil {
		push.Cancel()
		return result, err
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
		log.ErrorContext(ctx, "failed to collect live video metrics", slog.Any("error", metricsErr))
		return result, metricsErr
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

	result.Metrics = metrics
	return result, nil
}

func (c *client) collectLiveVideoMetrics(ctx context.Context, playbackURL string, duration time.Duration, ingestStart time.Time) (*Metrics, error) {
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	timer := time.AfterFunc(duration, cancelProbe)
	defer timer.Stop()

	go func() {
		<-ctx.Done()
		cancelProbe()
	}()

	args := buildPlaybackProbeArgs(playbackURL)

	c.probeLogger.DebugContext(ctx, "playback probe command", slog.String("args", strings.Join(args, " ")))

	cmd := exec.CommandContext(probeCtx, "ffmpeg", args...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.probeLogger.ErrorContext(ctx, "failed to get playback probe stderr", slog.Any("error", err))
		cancelProbe()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		c.probeLogger.ErrorContext(ctx, "failed to start playback probe", slog.Any("error", err))
		cancelProbe()
		return nil, err
	}
	defer cancelProbe()

	metrics := newMetrics(ingestStart)

	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	for scanner.Scan() {
		line := scanner.Text()
		c.probeLogger.DebugContext(ctx, "raw playback probe output", slog.String("line", line))

		select {
		case <-ctx.Done():
			c.probeLogger.DebugContext(ctx, "context cancelled, stopping probe")
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
		c.probeLogger.ErrorContext(ctx, "error reading playback probe output", slog.Any("error", err))
		cancelProbe()
		<-waitCh
		return nil, err
	}

	waitErr := <-waitCh
	if waitErr != nil && errors.Is(waitErr, context.Canceled) && metrics.totalFrames > 0 {
		waitErr = nil
	}
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) && metrics.totalFrames == 0 {
		// If the playback reader failed and we have no frames, treat as an orchestrator issue.
		return nil, ErrProbeFailed
	}

	if metrics.totalFrames == 0 {
		// No frames received at all - likely an orchestrator issue
		return nil, fmt.Errorf("%w: %v", ErrProbeFailed, waitErr)
	}

	metrics.finalize(duration)
	return metrics, nil
}

func buildPlaybackProbeArgs(playbackURL string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "info",
		"-i", playbackURL,
		"-map", "0:v:0",
		"-vf", "showinfo",
		"-an",
		"-f", "null",
		"-",
	}
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
