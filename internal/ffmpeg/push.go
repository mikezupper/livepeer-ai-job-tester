package ffmpeg

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

type streamPush struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	errCh  chan error
	start  time.Time
	wOnce  sync.Once
	wErr   error
}

func startStreamPush(videoPath, mediaServerURL string, logger *slog.Logger) (*streamPush, error) {
	ctx, cancel := context.WithCancel(context.Background())

	args := []string{
		"-re", // Read input at the native frame rate. Useful for live streaming.
		"-stream_loop", "-1", // Loop the input video indefinitely (-1 means infinite loop).
		"-loglevel", "error", // Set the logging level to only show errors.
		"-i", videoPath, // Specify the input file path.
		"-c:v", "libx264", // Use the H.264 video codec for encoding.
		"-preset", "veryfast", // Use the "veryfast" preset for encoding speed vs. compression tradeoff.
		"-pix_fmt", "yuv420p", // Set the pixel format to YUV 4:2:0 planar.
		"-an", // Disable audio in the output stream.
		"-r", "30", // Set the output frame rate to 30 frames per second.
		"-g", "60", // Set the GOP (Group of Pictures) size to 60 frames.
		"-force_key_frames", "expr:gte(t,n_forced*2)", // Force keyframes every 2 seconds.
		"-vsync", "cfr", // Use constant frame rate (CFR) for output.
		"-f", "flv", // Set the output format to FLV (Flash Video).
		mediaServerURL, // Specify the output URL (e.g., RTMP server).
	}

	log := logger
	if log == nil {
		log = slog.Default()
	}

	log.Debug("starting ffmpeg", slog.Any("args", args))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = io.Discard

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	pipeProcessOutput(ctx, stderr, log, "ffmpeg")

	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Wait() }()

	return &streamPush{
		cmd:    cmd,
		cancel: cancel,
		errCh:  errCh,
		start:  startTime,
	}, nil
}

func (p *streamPush) Cancel() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *streamPush) ErrChan() <-chan error { return p.errCh }

func (p *streamPush) StartTime() time.Time { return p.start }

func (p *streamPush) Cmd() *exec.Cmd { return p.cmd }

func (p *streamPush) Wait() error {
	p.wOnce.Do(func() {
		if p.errCh == nil {
			return
		}
		p.wErr = <-p.errCh
	})
	return p.wErr
}
