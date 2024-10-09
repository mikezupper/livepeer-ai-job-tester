package services

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/types"
	"net/http"
)

// LivepeerService defines the interface for interacting with the Livepeer Gateway and Leaderboard API.
// It includes methods to fetch orchestrators, fetch pipelines, and post stats.
type LivepeerService interface {
	FetchOrchestrators() ([]types.Orchestrator, error) // Fetches orchestrators from the Livepeer Gateway.
	FetchPipelines() (*types.Pipelines, error)         // Fetches pipeline data from the Livepeer Gateway.
	PostStats(stats *types.Stats) error                // Posts stats data to the Leaderboard API.
}

// HTTPLivepeerService is an implementation of the LivepeerService interface.
// It uses an HTTP client to make requests to the Livepeer Gateway and Leaderboard API.
type HTTPLivepeerService struct {
	client *http.Client   // HTTP client for making requests.
	config *config.Config // Configuration containing API endpoints and secrets.
}

// NewHTTPLivepeerService creates a new instance of HTTPLivepeerService with the given HTTP client and config.
// The returned service can be used to interact with the Livepeer Gateway and Leaderboard API.
func NewHTTPLivepeerService(client *http.Client, config *config.Config) *HTTPLivepeerService {
	return &HTTPLivepeerService{client: client, config: config}
}

// FetchOrchestrators fetches the list of registered orchestrators from the Livepeer Gateway.
// It filters out inactive orchestrators and those without a valid ServiceURI.
func (s *HTTPLivepeerService) FetchOrchestrators() ([]types.Orchestrator, error) {
	url := fmt.Sprintf("%s/registeredOrchestrators", s.config.BroadcasterCliEndpoint)
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[FetchOrchestrators] response contained a non-200 status code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var orchestrators []types.Orchestrator
	err = json.Unmarshal(body, &orchestrators)
	if err != nil {
		return nil, err
	}

	// Filter out orchestrators that are not active or have an empty ServiceURI.
	var filteredOrchestrators []types.Orchestrator
	for _, orchestrator := range orchestrators {
		if orchestrator.Active && orchestrator.ServiceURI != "" {
			filteredOrchestrators = append(filteredOrchestrators, orchestrator)
		}
	}

	return filteredOrchestrators, nil
}

// FetchPipelines fetches the available pipeline configurations from the Livepeer Gateway.
// The response contains the pipelines data, which is unmarshalled into the Pipelines struct.
func (s *HTTPLivepeerService) FetchPipelines() (*types.Pipelines, error) {
	url := fmt.Sprintf("%s/getNetworkCapabilities", s.config.BroadcasterCliEndpoint)
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[FetchPipelines] response contained a non-200 status code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var pipelines types.Pipelines
	err = json.Unmarshal(body, &pipelines)
	if err != nil {
		return nil, err
	}

	return &pipelines, nil
}

// PostStats posts job statistics to the Leaderboard API.
// The stats data is signed with an HMAC hash for authentication before being sent in a POST request.
func (s *HTTPLivepeerService) PostStats(stats *types.Stats) error {
	// Marshal the stats data into JSON format.
	input, err := json.Marshal(stats)
	if err != nil {
		return err
	}

	// Create a new POST request with the stats data.
	req, err := http.NewRequest("POST", s.config.MetricsApiEndpoint, bytes.NewBuffer(input))
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
		return errors.New(fmt.Sprintf("invalid response status code from POST STATS [%v]", res.StatusCode))
	}

	// Log the successful posting of stats.
	fmt.Printf("Posted stats for orchestrator %s - success=%v   latency=%v \n", stats.Orchestrator, stats.SuccessRate, stats.RoundTripTime)
	return nil
}
