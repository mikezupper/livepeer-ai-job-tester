package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// CaptureFrames stores a bounded set of still frames from the live playback URL.
//
// This is a best-effort debugging tool used only when explicitly enabled; the
// caller is expected to ignore failures when the stream is already gone.
func CaptureFrames(ctx context.Context, playbackURL, outputDir string, maxFrames int) ([]string, error) {
	if maxFrames <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create debug artifact directory: %w", err)
	}

	pattern := filepath.Join(outputDir, "frame-%03d.jpg")
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", playbackURL,
		"-vf", "fps=1",
		"-frames:v", strconv.Itoa(maxFrames),
		pattern,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("capture debug frames: %w (%s)", err, string(output))
	}

	var artifacts []string
	for i := 1; i <= maxFrames; i++ {
		path := filepath.Join(outputDir, fmt.Sprintf("frame-%03d.jpg", i))
		if _, err := os.Stat(path); err == nil {
			artifacts = append(artifacts, path)
		}
	}
	return artifacts, nil
}
