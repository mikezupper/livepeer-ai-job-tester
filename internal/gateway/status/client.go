package status

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var (
	ErrTimeout                  = errors.New("gateway readiness timeout")
	ErrOrchestratorBusy         = errors.New("gateway reported orchestrator busy")
	ErrNoOrchestratorsAvailable = errors.New("gateway reported no orchestrators available")
	ErrGatewayStreamFailed      = errors.New("gateway reported terminal stream failure")
)

// Client fetches and classifies live video status responses from the gateway.
type Client struct {
	httpClient *http.Client
	logger     *slog.Logger
}

// LiveStatusSnapshot captures the live status fields that the tester needs to
// classify busy/capacity failures and confirm the worker saw the intended params.
type LiveStatusSnapshot struct {
	StatusCode          int                 `json:"-"`
	RawJSON             string              `json:"-"`
	Type                string              `json:"type,omitempty"`
	Pipeline            string              `json:"pipeline,omitempty"`
	State               string              `json:"state,omitempty"`
	StartTime           int64               `json:"start_time,omitempty"`
	LastStatusTimestamp int64               `json:"last_status_timestamp,omitempty"`
	InputStatus         LiveInputStatus     `json:"input_status"`
	InferenceStatus     LiveInferenceStatus `json:"inference_status"`
	GatewayStatus       LiveGatewayStatus   `json:"gateway_status"`
}

type LiveInputStatus struct {
	LastInputTime int64   `json:"last_input_time,omitempty"`
	FPS           float64 `json:"fps,omitempty"`
}

type LiveInferenceStatus struct {
	LastOutputTime       int64                  `json:"last_output_time,omitempty"`
	FPS                  float64                `json:"fps,omitempty"`
	LastParamsUpdateTime int64                  `json:"last_params_update_time,omitempty"`
	LastParams           map[string]interface{} `json:"last_params,omitempty"`
	LastParamsHash       string                 `json:"last_params_hash,omitempty"`
	LastErrorTime        int64                  `json:"last_error_time,omitempty"`
	LastError            string                 `json:"last_error,omitempty"`
	LastRestartTime      int64                  `json:"last_restart_time,omitempty"`
	LastRestartLogs      []string               `json:"last_restart_logs,omitempty"`
	RestartCount         int                    `json:"restart_count,omitempty"`
}

type LiveGatewayStatus struct {
	WHEPURL string           `json:"whep_url,omitempty"`
	Error   *LiveStatusError `json:"error,omitempty"`
}

type LiveStatusError struct {
	ErrorMessage string `json:"error_message,omitempty"`
	ErrorTime    int64  `json:"error_time,omitempty"`
}

// NewClient constructs a status client with conservative HTTP defaults.
func NewClient(httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{httpClient: httpClient, logger: logger}
}

// WaitReady polls until the gateway reports a usable live status snapshot.
//
// A 200 response alone is not enough for debugging/classification; this method
// parses the body so capacity-related errors can be surfaced immediately.
func (c *Client) WaitReady(ctx context.Context, endpoint string, timeout, interval time.Duration) (*LiveStatusSnapshot, time.Duration, error) {
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
		snapshot, statusCode, err := c.Fetch(ctx, endpoint)
		if err != nil {
			c.logger.Debug("gateway status check failed", slog.String("endpoint", endpoint), slog.Any("error", err))
		} else if snapshot != nil {
			switch classifySnapshotError(snapshot) {
			case ErrOrchestratorBusy:
				return snapshot, time.Since(start), ErrOrchestratorBusy
			case ErrNoOrchestratorsAvailable:
				return snapshot, time.Since(start), ErrNoOrchestratorsAvailable
			case ErrGatewayStreamFailed:
				return snapshot, time.Since(start), ErrGatewayStreamFailed
			case nil:
				if statusCode == http.StatusOK {
					return snapshot, time.Since(start), nil
				}
			}
		} else if statusCode != 0 {
			c.logger.Debug("gateway stream not ready", slog.String("endpoint", endpoint), slog.Int("status_code", statusCode))
		}

		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-ticker.C:
		case <-timeoutTimerC(timeoutTimer):
			return nil, time.Since(start), ErrTimeout
		}
	}
}

// Fetch retrieves and parses a live status snapshot from the gateway.
func (c *Client) Fetch(ctx context.Context, endpoint string) (*LiveStatusSnapshot, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}

	body = bytesTrimSpace(body)
	if len(body) == 0 || string(body) == "null" {
		return &LiveStatusSnapshot{StatusCode: resp.StatusCode}, resp.StatusCode, nil
	}

	var snapshot LiveStatusSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, resp.StatusCode, err
	}
	snapshot.StatusCode = resp.StatusCode
	snapshot.RawJSON = string(body)
	return &snapshot, resp.StatusCode, nil
}

// ErrorMessage returns the most useful terminal error emitted by the live path.
func (s *LiveStatusSnapshot) ErrorMessage() string {
	if s == nil {
		return ""
	}
	if s.GatewayStatus.Error != nil && strings.TrimSpace(s.GatewayStatus.Error.ErrorMessage) != "" {
		return strings.TrimSpace(s.GatewayStatus.Error.ErrorMessage)
	}
	return strings.TrimSpace(s.InferenceStatus.LastError)
}

func classifySnapshotError(snapshot *LiveStatusSnapshot) error {
	message := strings.ToLower(snapshot.ErrorMessage())
	switch {
	case message == "":
		return nil
	case strings.Contains(message, "orchestratorcapped"),
		strings.Contains(message, "orchestratorbusy"),
		strings.Contains(message, "insufficient capacity"):
		return ErrOrchestratorBusy
	case strings.Contains(message, "no orchestrators available"):
		return ErrNoOrchestratorsAvailable
	default:
		return ErrGatewayStreamFailed
	}
}

func timeoutTimerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return neverC
	}
	return t.C
}

func bytesTrimSpace(data []byte) []byte {
	return []byte(strings.TrimSpace(string(data)))
}

var neverC <-chan time.Time
