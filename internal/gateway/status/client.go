package status

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

var (
	ErrTimeout = errors.New("gateway readiness timeout")
)

type Client struct {
	httpClient *http.Client
	logger     *slog.Logger
}

func NewClient(httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{httpClient: httpClient, logger: logger}
}

func (c *Client) WaitReady(ctx context.Context, endpoint string, timeout, interval time.Duration) (time.Duration, error) {
	if interval <= 0 {
		interval = time.Second
	}

	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var timeoutTimer *time.Timer
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		defer timeoutTimer.Stop()
	}

	for {
		ready, statusCode, err := c.check(ctx, endpoint)
		if ready {
			return time.Since(start), nil
		}

		if err != nil {
			c.logger.Debug("gateway status check failed", slog.String("endpoint", endpoint), slog.Any("error", err))
		} else if statusCode != 0 {
			c.logger.Debug("gateway stream not ready", slog.String("endpoint", endpoint), slog.Int("status_code", statusCode))
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		case <-timeoutTimerC(timeoutTimer):
			return time.Since(start), ErrTimeout
		}
	}
}

func (c *Client) check(ctx context.Context, endpoint string) (bool, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusOK {
		return true, resp.StatusCode, nil
	}

	return false, resp.StatusCode, nil
}

func timeoutTimerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return neverC
	}
	return t.C
}

var neverC <-chan time.Time
