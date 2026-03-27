package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"livepeer-job-tester/internal/config"
	status "livepeer-job-tester/internal/gateway/status"
	"livepeer-job-tester/internal/types"
)

type sequencingLivepeerService struct {
	pipelineResponses []*types.Pipelines
	pipelineCalls     int
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (s *sequencingLivepeerService) FetchOrchestrators(ctx context.Context) ([]types.Orchestrator, error) {
	return nil, nil
}

func (s *sequencingLivepeerService) FetchPipelines(ctx context.Context) (*types.Pipelines, error) {
	if len(s.pipelineResponses) == 0 {
		return &types.Pipelines{}, nil
	}
	if s.pipelineCalls >= len(s.pipelineResponses) {
		return s.pipelineResponses[len(s.pipelineResponses)-1], nil
	}
	resp := s.pipelineResponses[s.pipelineCalls]
	s.pipelineCalls++
	return resp, nil
}

func (s *sequencingLivepeerService) PostStats(ctx context.Context, stats *types.Stats) error {
	return nil
}

func liveCapabilityResponse(idleCapacity, capacityInUse int) *types.Pipelines {
	return &types.Pipelines{
		Orchestrators: []types.OrchestratorCapability{
			{
				Address: "0xorch",
				Pipelines: []types.Pipeline{
					{
						Type: "live-video-to-video",
						Models: []types.Model{
							{
								Name:          "streamdiffusion-sdxl-v2v",
								IdleCapacity:  idleCapacity,
								CapacityInUse: capacityInUse,
							},
						},
					},
				},
			},
		},
	}
}

func testLiveBundle() liveBundle {
	return liveBundle{
		OrchestratorAddress: "0xorch",
		ServiceURI:          "https://orch.example:8935",
		PipelineName:        "live-video-to-video",
		CapabilityModel: types.Model{
			Name: "streamdiffusion-sdxl-v2v",
		},
	}
}

func TestWaitForNextLivePromptReadinessReturnsReadyAfterRefresh(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		livepeerService: &sequencingLivepeerService{
			pipelineResponses: []*types.Pipelines{
				liveCapabilityResponse(0, 1),
				liveCapabilityResponse(1, 0),
			},
		},
		liveRetryPolicy: liveRetryPolicy{
			InterPromptSettleDelay:  1 * time.Millisecond,
			InterPromptReadyTimeout: 20 * time.Millisecond,
			InterPromptPollInterval: 1 * time.Millisecond,
			RetryCooldown:           50 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	capabilityModel, retryState, err := ss.waitForNextLivePromptReadiness(context.Background(), testLiveBundle())
	if err != nil {
		t.Fatalf("waitForNextLivePromptReadiness() error = %v", err)
	}
	if retryState != nil {
		t.Fatalf("waitForNextLivePromptReadiness() retryState = %#v, want nil", retryState)
	}
	if got, want := capabilityModel.IdleCapacity, 1; got != want {
		t.Fatalf("capabilityModel.IdleCapacity = %d, want %d", got, want)
	}
}

func TestWaitForNextLivePromptReadinessTimesOutIntoCooldown(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		livepeerService: &sequencingLivepeerService{
			pipelineResponses: []*types.Pipelines{
				liveCapabilityResponse(0, 1),
			},
		},
		liveRetryPolicy: liveRetryPolicy{
			InterPromptSettleDelay:  1 * time.Millisecond,
			InterPromptReadyTimeout: 5 * time.Millisecond,
			InterPromptPollInterval: 1 * time.Millisecond,
			RetryCooldown:           25 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	_, retryState, err := ss.waitForNextLivePromptReadiness(context.Background(), testLiveBundle())
	if err != nil {
		t.Fatalf("waitForNextLivePromptReadiness() error = %v", err)
	}
	if retryState == nil {
		t.Fatalf("waitForNextLivePromptReadiness() retryState = nil, want busy cooldown")
	}
	if got, want := retryState.DeferredOutcome, liveOutcomeDeferredBusy; got != want {
		t.Fatalf("retryState.DeferredOutcome = %q, want %q", got, want)
	}
	if !retryState.CooldownUntil.After(time.Now()) {
		t.Fatalf("retryState.CooldownUntil = %v, want future time", retryState.CooldownUntil)
	}
}

func TestReclassifyLiveFailureUsesFreshCapabilityData(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		livepeerService: &sequencingLivepeerService{
			pipelineResponses: []*types.Pipelines{
				liveCapabilityResponse(0, 1),
			},
		},
		liveRetryPolicy: liveRetryPolicy{
			InterPromptSettleDelay:  1 * time.Millisecond,
			InterPromptReadyTimeout: 5 * time.Millisecond,
			InterPromptPollInterval: 1 * time.Millisecond,
			RetryCooldown:           25 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	result := liveRunResult{
		Outcome: liveOutcomeFailed,
		RunErr:  context.DeadlineExceeded,
		TerminalStatus: &status.LiveStatusSnapshot{
			GatewayStatus: status.LiveGatewayStatus{
				Error: &status.LiveStatusError{ErrorMessage: "no orchestrators available"},
			},
		},
	}

	reclassified, retryState := ss.reclassifyLiveFailure(context.Background(), testLiveBundle(), result)
	if got, want := reclassified.Outcome, liveOutcomeDeferredBusy; got != want {
		t.Fatalf("reclassified.Outcome = %q, want %q", got, want)
	}
	if retryState == nil {
		t.Fatalf("reclassifyLiveFailure() retryState = nil, want busy cooldown")
	}
	if got, want := retryState.FinalOutcome, liveOutcomeUnscoredBusy; got != want {
		t.Fatalf("retryState.FinalOutcome = %q, want %q", got, want)
	}
}

func TestClassifyLivePromptStartupRetryReason(t *testing.T) {
	result := liveRunResult{
		Outcome: liveOutcomeFailed,
		RunErr:  errors.New("ffmpeg: stream aborted"),
		TerminalStatus: &status.LiveStatusSnapshot{
			GatewayStatus: status.LiveGatewayStatus{
				Error: &status.LiveStatusError{ErrorMessage: "no orchestrators available"},
			},
		},
	}

	if got, want := classifyLivePromptStartupRetryReason(result), "no orchestrators available"; got != want {
		t.Fatalf("classifyLivePromptStartupRetryReason() = %q, want %q", got, want)
	}
}

func TestExecuteLiveRunWithStartupRetryRetriesEarlyCapacityFailure(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		livepeerService: &sequencingLivepeerService{
			pipelineResponses: []*types.Pipelines{
				liveCapabilityResponse(1, 0),
			},
		},
		liveRetryPolicy: liveRetryPolicy{
			InterPromptSettleDelay:      0,
			InterPromptReadyTimeout:     5 * time.Millisecond,
			InterPromptPollInterval:     1 * time.Millisecond,
			RetryCooldown:               25 * time.Millisecond,
			StartupRetryAttempts:        2,
			StartupRetryBaseDelay:       1 * time.Millisecond,
			StartupRetryTeardownTimeout: 1 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	attempts := 0
	streamIDs := make([]string, 0, 2)
	run := func(spec liveRunSpec, _ types.Model) liveRunResult {
		attempts++
		streamIDs = append(streamIDs, spec.StreamID)
		if attempts == 1 {
			return liveRunResult{
				Outcome: liveOutcomeFailed,
				RunErr:  errors.New("ffmpeg: stream aborted"),
				TerminalStatus: &status.LiveStatusSnapshot{
					GatewayStatus: status.LiveGatewayStatus{
						Error: &status.LiveStatusError{ErrorMessage: "no orchestrators available"},
					},
				},
			}
		}
		return liveRunResult{Outcome: liveOutcomePassed}
	}

	spec := liveRunSpec{PromptID: "cyberpunk-medium", StreamID: "stream-attempt-1", StatusEndpoint: "http://status/stream-attempt-1"}
	nextSpecID := 1
	buildNextSpec := func() (liveRunSpec, error) {
		nextSpecID++
		return liveRunSpec{
			PromptID:       "cyberpunk-medium",
			StreamID:       "stream-attempt-" + strconv.Itoa(nextSpecID),
			StatusEndpoint: "http://status/stream-attempt-" + strconv.Itoa(nextSpecID),
		}, nil
	}

	finalSpec, result, capabilityModel, err := ss.executeLiveRunWithStartupRetryFunc(context.Background(), testLiveBundle(), spec, types.Model{}, run, buildNextSpec)
	if err != nil {
		t.Fatalf("executeLiveRunWithStartupRetry() error = %v", err)
	}
	if got, want := attempts, 2; got != want {
		t.Fatalf("run attempts = %d, want %d", got, want)
	}
	if got, want := result.Outcome, liveOutcomePassed; got != want {
		t.Fatalf("result.Outcome = %q, want %q", got, want)
	}
	if got, want := capabilityModel.IdleCapacity, 1; got != want {
		t.Fatalf("capabilityModel.IdleCapacity = %d, want %d", got, want)
	}
	if got, want := finalSpec.StreamID, "stream-attempt-2"; got != want {
		t.Fatalf("finalSpec.StreamID = %q, want %q", got, want)
	}
	if len(streamIDs) != 2 || streamIDs[0] == streamIDs[1] {
		t.Fatalf("streamIDs = %#v, want distinct IDs per retry", streamIDs)
	}
}

func TestExecuteLiveRunWithStartupRetryStopsAfterBoundedAttempts(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		livepeerService: &sequencingLivepeerService{
			pipelineResponses: []*types.Pipelines{
				liveCapabilityResponse(1, 0),
			},
		},
		liveRetryPolicy: liveRetryPolicy{
			InterPromptSettleDelay:      0,
			InterPromptReadyTimeout:     5 * time.Millisecond,
			InterPromptPollInterval:     1 * time.Millisecond,
			RetryCooldown:               25 * time.Millisecond,
			StartupRetryAttempts:        2,
			StartupRetryBaseDelay:       1 * time.Millisecond,
			StartupRetryTeardownTimeout: 1 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	attempts := 0
	run := func(liveRunSpec, types.Model) liveRunResult {
		attempts++
		return liveRunResult{
			Outcome: liveOutcomeFailed,
			RunErr:  errors.New("ffmpeg: stream aborted"),
			TerminalStatus: &status.LiveStatusSnapshot{
				GatewayStatus: status.LiveGatewayStatus{
					Error: &status.LiveStatusError{ErrorMessage: "insufficient capacity"},
				},
			},
		}
	}

	spec := liveRunSpec{PromptID: "cyberpunk-medium", StreamID: "stream-attempt-1", StatusEndpoint: "http://status/stream-attempt-1"}
	nextSpecID := 1
	buildNextSpec := func() (liveRunSpec, error) {
		nextSpecID++
		return liveRunSpec{
			PromptID:       "cyberpunk-medium",
			StreamID:       "stream-attempt-" + strconv.Itoa(nextSpecID),
			StatusEndpoint: "http://status/stream-attempt-" + strconv.Itoa(nextSpecID),
		}, nil
	}

	_, result, _, err := ss.executeLiveRunWithStartupRetryFunc(context.Background(), testLiveBundle(), spec, types.Model{}, run, buildNextSpec)
	if err != nil {
		t.Fatalf("executeLiveRunWithStartupRetry() error = %v", err)
	}
	if got, want := attempts, 3; got != want {
		t.Fatalf("run attempts = %d, want %d", got, want)
	}
	if got, want := result.Outcome, liveOutcomeDeferredBusy; got != want {
		t.Fatalf("result.Outcome = %q, want %q", got, want)
	}
}

func TestWaitForFailedLiveStreamTeardownStopsOnTerminalStatus(t *testing.T) {
	requests := 0
	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"gateway_status":{"error":{"error_message":"no orchestrators available"}}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	ss := &EmbeddedWebhookServer{
		statusClient: status.NewClient(httpClient, slog.Default()),
		liveRetryPolicy: liveRetryPolicy{
			InterPromptPollInterval:     1 * time.Millisecond,
			StartupRetryTeardownTimeout: 20 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	if err := ss.waitForFailedLiveStreamTeardown(context.Background(), "http://status.example/stream", ss.resolvedLiveRetryPolicy()); err != nil {
		t.Fatalf("waitForFailedLiveStreamTeardown() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("status requests = %d, want 1 because terminal status should stop teardown polling immediately", requests)
	}
}

func TestExecuteLiveRunWithStartupRetryBuildsFreshSpecs(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		config: &config.Config{
			BroadcasterJobEndpoint: "http://gateway.example",
			LiveVideo: &config.LiveVideoConfig{
				MediaServerURL: "rtmp://mediamtx:1935/{streamKey}",
			},
			Pipelines: []config.Pipeline{
				{
					Uri:  "live-video-to-video",
					Live: true,
				},
			},
		},
		livepeerService: &sequencingLivepeerService{
			pipelineResponses: []*types.Pipelines{
				liveCapabilityResponse(1, 0),
			},
		},
		liveRetryPolicy: liveRetryPolicy{
			InterPromptPollInterval:     1 * time.Millisecond,
			StartupRetryAttempts:        1,
			StartupRetryBaseDelay:       1 * time.Millisecond,
			StartupRetryTeardownTimeout: 1 * time.Millisecond,
		},
		logger: slog.Default(),
	}

	promptJob := livePromptJob{
		Variant: config.PromptVariant{
			ID:         "cyberpunk-medium",
			Complexity: "medium",
			Parameters: map[string]interface{}{"prompt": "cyberpunk city"},
		},
	}

	bundle := testLiveBundle()
	spec, err := ss.buildLiveRunSpec(bundle, promptJob)
	if err != nil {
		t.Fatalf("buildLiveRunSpec() error = %v", err)
	}

	streamIDs := make([]string, 0, 2)
	run := func(spec liveRunSpec, _ types.Model) liveRunResult {
		streamIDs = append(streamIDs, spec.StreamID)
		if len(streamIDs) == 1 {
			return liveRunResult{
				Outcome: liveOutcomeFailed,
				RunErr:  errors.New("ffmpeg: stream aborted"),
				TerminalStatus: &status.LiveStatusSnapshot{
					GatewayStatus: status.LiveGatewayStatus{
						Error: &status.LiveStatusError{ErrorMessage: "no orchestrators available"},
					},
				},
			}
		}
		return liveRunResult{Outcome: liveOutcomePassed}
	}

	finalSpec, result, _, err := ss.executeLiveRunWithStartupRetryFunc(
		context.Background(),
		bundle,
		spec,
		types.Model{},
		run,
		func() (liveRunSpec, error) {
			return ss.buildLiveRunSpec(bundle, promptJob)
		},
	)
	if err != nil {
		t.Fatalf("executeLiveRunWithStartupRetry() error = %v", err)
	}
	if got, want := result.Outcome, liveOutcomePassed; got != want {
		t.Fatalf("result.Outcome = %q, want %q", got, want)
	}
	if got, want := len(streamIDs), 2; got != want {
		t.Fatalf("run attempts = %d, want %d", got, want)
	}
	if streamIDs[0] == streamIDs[1] {
		t.Fatalf("streamIDs = %#v, want a fresh stream ID on retry", streamIDs)
	}
	if finalSpec.StreamID != streamIDs[1] {
		t.Fatalf("finalSpec.StreamID = %q, want %q", finalSpec.StreamID, streamIDs[1])
	}
}
