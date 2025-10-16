package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/types"
)

// LivepeerService defines the interface for interacting with the Livepeer Gateway and Leaderboard API.
// It includes methods to fetch orchestrators, fetch pipelines, and post stats.
type LivepeerService interface {
	FetchOrchestrators(ctx context.Context) ([]types.Orchestrator, error) // Fetches orchestrators from the Livepeer Gateway.
	FetchPipelines(ctx context.Context) (*types.Pipelines, error)         // Fetches pipeline data from the Livepeer Gateway.
	PostStats(ctx context.Context, stats *types.Stats) error              // Posts stats data to the Leaderboard API.
}

// HTTPLivepeerService is an implementation of the LivepeerService interface.
// It uses an HTTP client to make requests to the Livepeer Gateway and Leaderboard API.
type HTTPLivepeerService struct {
	client *http.Client   // HTTP client for making requests.
	config *config.Config // Configuration containing API endpoints and secrets.
	logger *slog.Logger
}

// NewHTTPLivepeerService creates a new instance of HTTPLivepeerService with the given HTTP client and config.
// The returned service can be used to interact with the Livepeer Gateway and Leaderboard API.
func NewHTTPLivepeerService(client *http.Client, config *config.Config, logger *slog.Logger) *HTTPLivepeerService {
	if logger == nil {
		logger = slog.Default()
	}
	return &HTTPLivepeerService{client: client, config: config, logger: logger}
}

// FetchOrchestrators fetches the list of registered orchestrators from the Livepeer Gateway.
// It filters out inactive orchestrators, those without a valid ServiceURI, and applies any test mode filtering based on the configuration.
func (s *HTTPLivepeerService) FetchOrchestrators(ctx context.Context) ([]types.Orchestrator, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	s.logger.InfoContext(ctx, "fetching registered orchestrators")
	url := fmt.Sprintf("%s/registeredOrchestrators", s.config.BroadcasterCliEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetchOrchestrators: unexpected status code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var orchestrators []types.Orchestrator
	if err := json.Unmarshal(body, &orchestrators); err != nil {
		return nil, err
	}

	var filtered []types.Orchestrator
	for _, orchestrator := range orchestrators {
		if orchestrator.Active && orchestrator.ServiceURI != "" {
			filtered = append(filtered, orchestrator)
		}
	}

	s.logger.InfoContext(ctx, "orchestrators filtered",
		slog.Int("fetched", len(orchestrators)),
		slog.Int("active", len(filtered)))

	return filtered, nil
}

// FetchPipelines fetches the available pipeline configurations from the Livepeer Gateway.
// The response contains the pipelines data, which is unmarshalled into the Pipelines struct.
func (s *HTTPLivepeerService) FetchPipelines(ctx context.Context) (*types.Pipelines, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	url := fmt.Sprintf("%s/getOrchestratorAICapabilities", s.config.BroadcasterCliEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetchPipelines: unexpected status code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var pipelines types.Pipelines
	if err := json.Unmarshal(body, &pipelines); err != nil {
		return nil, err
	}

	s.logger.DebugContext(ctx, "pipelines fetched", slog.Int("orchestrators", len(pipelines.Orchestrators)))

	return &pipelines, nil
}

// PostStats posts job statistics to the Leaderboard API.
// The stats data is signed with an HMAC hash for authentication before being sent in a POST request.

func (s *HTTPLivepeerService) PostStats(ctx context.Context, stats *types.Stats) error {
	if ctx == nil {
		ctx = context.Background()
	}
	
	// Marshal the stats data into JSON format.
	input, err := json.Marshal(stats)
	if err != nil {
		return err
	}

	// Create a new POST request with the stats data.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.MetricsApiEndpoint, bytes.NewBuffer(input))
	if err != nil {
		return err
	}

	// Generate an HMAC hash using the metrics secret and the request body.
	hash := hmac.New(sha256.New, []byte(s.config.MetricsSecret))
	hash.Write(input)
	req.Header.Set("Authorization", hex.EncodeToString(hash.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")

	// Send the POST request.
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// Check the response status code.
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("invalid response status code from POST STATS [%v]", res.StatusCode)
	}

	// Log the successful posting of stats.
	s.logger.InfoContext(ctx, "posted stats",
		slog.String("region", stats.Region),
		slog.String("orchestrator", stats.Orchestrator),
		slog.String("pipeline", stats.Pipeline),
		slog.String("model", stats.Model),
		slog.Float64("success_rate", float64(stats.SuccessRate)),
		slog.Float64("round_trip", stats.RoundTripTime))
	return nil
}
