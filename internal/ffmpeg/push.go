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
		"-re",
		"-stream_loop", "-1",
		"-loglevel", "error",
		"-i", videoPath,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-an",
		"-r", "30",
		"-g", "60",
		"-force_key_frames", "expr:gte(t,n_forced*2)",
		"-vsync", "cfr",
		"-f", "flv",
		mediaServerURL,
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
