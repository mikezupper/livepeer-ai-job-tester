package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/ffmpeg"
	status "livepeer-job-tester/internal/gateway/status"
	"livepeer-job-tester/internal/services"
	"livepeer-job-tester/internal/types"
)

const (
	startupGatewayFetchAttempts = 15
	startupGatewayFetchDelay    = 5 * time.Second
)

// EmbeddedWebhookServer represents the server responsible for managing job testing and orchestrator interactions.
// It contains configuration, a client, and a metrics service for tracking job test results.
type EmbeddedWebhookServer struct {
	config                *config.Config             // Configuration for the server, including API endpoints and credentials.
	livepeerService       services.LivepeerService   // Service to interact with Livepeer API for fetching orchestrators and pipelines.
	client                *http.Client               // HTTP client for making requests.
	jobTesterMetrics      *services.JobTesterMetrics // Metrics service for tracking job tester results.
	ffmpegClient          ffmpeg.Client
	statusClient          *status.Client
	liveManualAttachDelay time.Duration
	liveRetryPolicy       liveRetryPolicy
	logger                *slog.Logger
}

// RuntimeOptions controls local/debug runtime behavior that should not be baked into shared config files.
type RuntimeOptions struct {
	LiveManualAttachDelay time.Duration
}

// NewEmbeddedWebhookServer creates a new instance of EmbeddedWebhookServer with the provided configuration, HTTP client, and Livepeer service.
func NewEmbeddedWebhookServer(
	config *config.Config,
	client *http.Client,
	livepeerService services.LivepeerService,
	ffmpegClient ffmpeg.Client,
	statusClient *status.Client,
	runtimeOptions RuntimeOptions,
	logger *slog.Logger,
) (*EmbeddedWebhookServer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if ffmpegClient == nil {
		return nil, errors.New("ffmpeg client is required")
	}
	if statusClient == nil {
		return nil, errors.New("status client is required")
	}

	return &EmbeddedWebhookServer{
		config:                config,
		client:                client,
		livepeerService:       livepeerService,
		ffmpegClient:          ffmpegClient,
		statusClient:          statusClient,
		liveManualAttachDelay: runtimeOptions.LiveManualAttachDelay,
		liveRetryPolicy:       defaultLiveRetryPolicy,
		logger:                logger,
		jobTesterMetrics:      services.NewJobTesterMetrics(),
	}, nil
}

// RunTestJobs fetches orchestrators and pipelines from the Livepeer API and sends test jobs to each orchestrator.
// It increments job metrics and generates a JSON report of the job tester results.
func (ss *EmbeddedWebhookServer) RunTestJobs(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// The tester often starts alongside the Gateway in local docker-compose runs.
	// A small bounded retry window avoids failing immediately when the Gateway
	// container exists but has not bound its CLI endpoints yet.
	pipelines, err := ss.fetchPipelinesWithRetry(ctx)
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("failed to fetch pipelines: %w", err)
	}

	ss.logger.InfoContext(ctx, "orchestrators fetched", slog.Int("count", len(pipelines.Orchestrators)))
	for _, cap := range pipelines.Orchestrators {
		ss.logger.DebugContext(ctx, "capabilities loaded",
			slog.String("orchestrator", cap.Address),
			slog.Int("pipelines", len(cap.Pipelines)))
	}

	standardJobs, liveBundles := ss.buildExecutionPlan(ctx, pipelines.Orchestrators)
	ss.logger.InfoContext(ctx, "expected jobs computed", slog.Int("total", ss.jobTesterMetrics.ExpectedTotalJobs))

	for _, job := range standardJobs {
		ss.logger.InfoContext(ctx, "sending test job",
			slog.String("region", ss.config.Region),
			slog.String("orchestrator", job.OrchestratorAddress),
			slog.String("service_uri", job.ServiceURI),
			slog.String("pipeline", job.PipelineName),
			slog.String("model", job.Model.Name),
			slog.Bool("warm", job.Model.Status.Warm > 0))

		if err := ss.SendTestJob(ctx, job.OrchestratorAddress, job.ServiceURI, job.PipelineName, job.Model.Name, job.Model.Status.Warm > 0); err != nil {
			ss.logger.ErrorContext(ctx, "failed to send test job",
				slog.String("region", ss.config.Region),
				slog.String("orchestrator", job.OrchestratorAddress),
				slog.String("service_uri", job.ServiceURI),
				slog.String("pipeline", job.PipelineName),
				slog.String("model", job.Model.Name),
				slog.Any("error", err))
		}
	}

	if err := ss.runLiveBundles(ctx, liveBundles); err != nil {
		return err
	}

	ss.logger.InfoContext(ctx, "test jobs completed", slog.Int("total_jobs", ss.jobTesterMetrics.TotalJobs))

	statsJSON, err := json.Marshal(ss.jobTesterMetrics)
	if err != nil {
		ss.logger.ErrorContext(ctx, "failed to marshal job stats", slog.Any("error", err))
		return err
	}
	ss.logger.InfoContext(ctx, "job stats report", slog.String("payload", string(statsJSON)))
	return nil
}

func (ss *EmbeddedWebhookServer) fetchPipelinesWithRetry(ctx context.Context) (*types.Pipelines, error) {
	var lastErr error
	for attempt := 1; attempt <= startupGatewayFetchAttempts; attempt++ {
		pipelines, err := ss.livepeerService.FetchPipelines(ctx)
		if err == nil && len(pipelines.Orchestrators) > 0 {
			if attempt > 1 {
				ss.logger.InfoContext(ctx, "gateway capability endpoint became ready",
					slog.Int("attempt", attempt),
					slog.Int("orchestrators", len(pipelines.Orchestrators)))
			}
			return pipelines, nil
		}

		if err == nil {
			lastErr = fmt.Errorf("gateway returned 0 orchestrators")
		} else {
			lastErr = err
		}

		if attempt == startupGatewayFetchAttempts {
			break
		}

		ss.logger.WarnContext(ctx, "gateway capability endpoint not ready yet",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", startupGatewayFetchAttempts),
			slog.Duration("retry_delay", startupGatewayFetchDelay),
			slog.Any("error", lastErr))

		if err := sleepWithContext(ctx, startupGatewayFetchDelay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// buildExecutionPlan expands the raw capability snapshot into non-live jobs and
// live-video bundles. The live bundles keep all prompt variants for an orch/model
// together so capacity-based deferrals can move the whole orch bundle to a later pass.
func (ss *EmbeddedWebhookServer) buildExecutionPlan(ctx context.Context, caps []types.OrchestratorCapability) ([]standardJob, []liveBundle) {
	var standardJobs []standardJob
	var liveBundles []liveBundle

	for _, cap := range caps {
		for _, pipeline := range cap.Pipelines {
			pipelineName := pipeline.Type
			urisToTest, overrideApplied := ss.resolveServiceURIs(cap, pipelineName)
			if len(urisToTest) == 0 {
				ss.logger.WarnContext(ctx, "no service URIs resolved for orchestrator pipeline",
					slog.String("orchestrator", cap.Address),
					slog.String("pipeline", pipelineName))
				continue
			}

			cfgPipeline, found := ss.findParametersByPipelineName(pipelineName)
			if !found {
				ss.logger.WarnContext(ctx, "pipeline missing config", slog.String("pipeline", pipelineName))
				continue
			}

			for _, model := range pipeline.Models {
				for _, uri := range urisToTest {
					if overrideApplied {
						ss.logger.DebugContext(ctx, "overriding service URI for orchestrator",
							slog.String("orchestrator", cap.Address),
							slog.String("service_uri", uri),
							slog.String("pipeline", pipelineName))
					}

					if cfgPipeline.Live && pipelineName == "live-video-to-video" {
						bundle := liveBundle{
							OrchestratorAddress: cap.Address,
							ServiceURI:          uri,
							PipelineName:        pipelineName,
							CapabilityModel:     model,
							Prompts:             make([]livePromptJob, 0, len(cfgPipeline.PromptVariants)),
						}
						for _, variant := range cfgPipeline.PromptVariants {
							ss.jobTesterMetrics.IncrementExpectedTotalJobs()
							bundle.Prompts = append(bundle.Prompts, livePromptJob{
								Variant:     variant,
								ModelIsWarm: model.Status.Warm > 0,
							})
							ss.logger.DebugContext(ctx, "queued live prompt job",
								slog.String("orchestrator", cap.Address),
								slog.String("service_uri", uri),
								slog.String("pipeline", pipelineName),
								slog.String("model", model.Name),
								slog.String("prompt_id", variant.ID),
								slog.String("prompt_complexity", variant.Complexity))
						}
						liveBundles = append(liveBundles, bundle)
						continue
					}

					ss.jobTesterMetrics.IncrementExpectedTotalJobs()
					standardJobs = append(standardJobs, standardJob{
						OrchestratorAddress: cap.Address,
						ServiceURI:          uri,
						PipelineName:        pipelineName,
						Model:               model,
					})
					ss.logger.DebugContext(ctx, "queued test job",
						slog.String("orchestrator", cap.Address),
						slog.String("service_uri", uri),
						slog.String("pipeline", pipelineName),
						slog.String("model", model.Name))
				}
			}
		}
	}

	return standardJobs, liveBundles
}

func (ss *EmbeddedWebhookServer) resolveServiceURIs(cap types.OrchestratorCapability, pipelineName string) ([]string, bool) {
	uri := strings.TrimSpace(cap.ServiceURI)
	if uri == "" {
		return nil, false
	}

	ss.logger.Debug("resolved service URI from network capabilities",
		slog.String("orchestrator", cap.Address),
		slog.String("pipeline", pipelineName),
		slog.String("service_uri", uri))
	return []string{uri}, false
}

// SendTestJob sends a test job to the specified orchestrator and pipeline, including the model name and warm status.
// It updates the job tester metrics and processes the response, handling errors and capturing response data.
func (ss *EmbeddedWebhookServer) SendTestJob(ctx context.Context, orchEthAddr, orchServiceUri, pipeline, model string, modelIsWarm bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	ss.jobTesterMetrics.IncrementTotalJobs()

	// Find pipeline parameters from the config.
	cfgPipeline, found := ss.findParametersByPipelineName(pipeline)
	if !found {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("pipeline not found in configuration file: %s", pipeline)
	}

	// Copy pipeline parameters and add the model ID and pipeline.
	copiedParams := make(map[string]any)
	for key, value := range cfgPipeline.Parameters {
		copiedParams[key] = value
	}
	copiedParams["model_id"] = model
	copiedParams["pipeline"] = model
	if os.Getenv("TEST_INDIVIDUAL_ORCHESTRATORS") != "false" {
		copiedParams["orchestrator"] = orchServiceUri
	}

	// Marshal the parameters into JSON format.
	input, err := json.Marshal(copiedParams)
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("failed to create job parameters for pipeline %s: %w", pipeline, err)
	}

	// Initialize stats for the test job.
	stats := types.Stats{
		Region:       ss.config.Region,
		Pipeline:     pipeline,
		Model:        model,
		ModelIsWarm:  modelIsWarm,
		Orchestrator: orchEthAddr,
		Timestamp:    time.Now().Unix(),
		SuccessRate:  0,
		Errors:       make([]types.Error, 0),
	}
	stats.InputParameters = string(input)

	ss.logger.InfoContext(ctx, "sending test job to Orchestrator",
		slog.String("orchestrator_uri", orchServiceUri),
		slog.String("pipeline", pipeline),
		slog.String("model", model),
		slog.Bool("live", cfgPipeline.Live))

	if cfgPipeline.Live {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("live pipelines must be executed through the live bundle scheduler")
	} else {

		// Non-live job submission
		// Send the HTTP request
		var url string
		if os.Getenv("TEST_INDIVIDUAL_ORCHESTRATORS") != "false" {
			url = fmt.Sprintf("%s/%s", ss.config.BroadcasterJobEndpoint, cfgPipeline.Uri)
		} else {
			url = fmt.Sprintf("%s/%s?orchestrator=%s", ss.config.BroadcasterJobEndpoint, cfgPipeline.Uri, orchServiceUri)
		}
		var req *http.Request
		if cfgPipeline.ContentType == "application/json" {
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(input))
			if err != nil {
				ss.jobTesterMetrics.IncrementTotalJobsTesterError()
				return fmt.Errorf("failed to create new HTTP request: %w", err)
			}
			req.Header.Set("Content-Type", cfgPipeline.ContentType)
			req.Header.Set("Authorization", "Bearer "+ss.config.BroadcasterRequestToken)
		} else {
			req, err = ss.createMultipartRequest(url, copiedParams, cfgPipeline.Uri)
			if err != nil {
				ss.jobTesterMetrics.IncrementTotalJobsTesterError()
				return fmt.Errorf("failed to create multipart request: %w", err)
			}
			req = req.WithContext(ctx)
		}

		// Measure round-trip time.
		startTime := time.Now()
		res, err := ss.client.Do(req)
		jobTime := time.Now()

		// Handle request errors.
		if err != nil {
			stats.RoundTripTime = jobTime.Sub(startTime).Seconds()
			return ss.handleRequestError(ctx, err, "failed to process the job", &stats)
		}
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		readBodyTime := time.Now()
		if err != nil {
			stats.RoundTripTime = readBodyTime.Sub(startTime).Seconds()
			return ss.handleRequestError(ctx, err, "failed to read response body", &stats)
		}
		stats.RoundTripTime = readBodyTime.Sub(startTime).Seconds()

		// Check status code and handle errors.
		if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
			//capture the error response from gateway
			stats.ResponsePayload = string(body)
			return ss.handleStatusCodeError(ctx, res.StatusCode, string(body), &stats)
		}

		// Capture response if necessary.
		if cfgPipeline.CaptureResponse {
			stats.ResponsePayload = string(body)
		} else {
			stats.ResponsePayload = "{\"message\":\"(Job Tester) Capture Response Disabled\"}"
		}

		// Finalize stats and report success.
		return ss.handleSuccess(ctx, &stats)
	}
}

func buildLiveStatusEndpoint(base, streamKey string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(base), "/")
	return fmt.Sprintf("%s/live/video-to-video/%s/status", trimmed, streamKey)
}

func applyMetricsToStats(stats *types.Stats, metrics *ffmpeg.Metrics, cfg *config.LiveVideoConfig) {
	if stats == nil || metrics == nil {
		return
	}
	stats.ResponsePayload = fmt.Sprintf(`{"message":"Live Video Test Completed","total_frames":%d,"average_fps":%.2f,"average_latency":%.2f,"test_duration":%.2f,"initial_latency":%.2f,"gateway_ready_secs":%.2f}`,
		metrics.TotalFrames(),
		metrics.AverageFPS(),
		metrics.AverageLatency(),
		metrics.DurationSeconds(),
		metrics.InitialLatency(),
		metrics.GatewayReadySeconds(),
	)

	var targetFPS, maxInitialLatency float64
	if cfg != nil {
		targetFPS = cfg.TargetFPS
		maxInitialLatency = cfg.MaxInitialLatencySeconds
	}
	metricsScore := metrics.Score(targetFPS, maxInitialLatency)
	stats.RoundTripTime = metricsScore
}

// findParametersByPipelineName searches for a pipeline by name in the configuration file.
func (ss *EmbeddedWebhookServer) findParametersByPipelineName(pipelineName string) (*config.Pipeline, bool) {
	for _, pipeline := range ss.config.Pipelines {
		if pipeline.Uri == pipelineName {
			return &pipeline, true
		}
	}
	return nil, false
}

// createMultipartRequest creates a new multipart/form-data request for pipelines that require file uploads.
func (ss *EmbeddedWebhookServer) createMultipartRequest(url string, params map[string]interface{}, uri string) (*http.Request, error) {
	// Prepare the multipart form data.
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	// Add fields to the form.
	for key, value := range params {
		_ = writer.WriteField(key, fmt.Sprintf("%v", value))
	}

	// Add the file based on the URI.
	var testFileName, fileFieldName string
	switch uri {
	case "audio-to-text":
		testFileName = "test-assets/test-audio.mp4"
		fileFieldName = "audio"
	case "upscale":
		testFileName = "test-assets/test-upscale.jpg"
		fileFieldName = "image"
	default:
		testFileName = "test-assets/test-image.png"
		fileFieldName = "image"
	}

	file, err := os.Open(testFileName)
	if err != nil {
		return nil, fmt.Errorf("Error opening file: %v", err)
	}
	defer file.Close()

	part, err := writer.CreateFormFile(fileFieldName, file.Name())
	if err != nil {
		return nil, fmt.Errorf("Error creating form file: %v", err)
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return nil, fmt.Errorf("Error copying image to form file: %v", err)
	}

	// Close the multipart writer to set the terminating boundary.
	err = writer.Close()
	if err != nil {
		return nil, fmt.Errorf("Error closing writer: %v", err)
	}

	req, err := http.NewRequest("POST", url, &buffer)
	if err != nil {
		return nil, fmt.Errorf("[createMultipartRequest] failed to get response for POST test: %v", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+ss.config.BroadcasterRequestToken)
	return req, nil
}

// handleRequestError handles errors that occur while processing a request.
// It updates job stats and posts the error data to the Leaderboard API.

func (ss *EmbeddedWebhookServer) handleRequestError(ctx context.Context, err error, message string, stats *types.Stats) error {
	newError := types.Error{
		ErrorCode: fmt.Errorf("%w", err).Error(),
		Message:   message,
		Count:     1,
	}
	stats.Errors = append(stats.Errors, newError)
	ss.jobTesterMetrics.IncrementTotalJobsFailed()
	ss.logger.ErrorContext(ctx, message,
		slog.Any("error", err),
		slog.String("pipeline", stats.Pipeline),
		slog.String("model", stats.Model),
		slog.String("orchestrator", stats.Orchestrator))
	return ss.livepeerService.PostStats(ctx, stats)
}

// handleSuccess handles successful completion of a test job by updating job stats and posting them to the Leaderboard API.

func (ss *EmbeddedWebhookServer) handleSuccess(ctx context.Context, stats *types.Stats) error {
	stats.SuccessRate = 1
	ss.jobTesterMetrics.IncrementTotalJobsPassed()
	ss.logger.InfoContext(ctx, "job succeeded",
		slog.String("pipeline", stats.Pipeline),
		slog.String("model", stats.Model),
		slog.String("orchestrator", stats.Orchestrator))
	return ss.livepeerService.PostStats(ctx, stats)
}

// handleStatusCodeError handles errors related to non-2xx status codes in HTTP responses.
// It updates job stats and posts the error data to the Leaderboard API.
func (ss *EmbeddedWebhookServer) handleStatusCodeError(ctx context.Context, statusCode int, message string, stats *types.Stats) error {
	newError := types.Error{
		ErrorCode: strconv.Itoa(statusCode),
		Message:   message,
		Count:     1,
	}
	stats.Errors = append(stats.Errors, newError)
	ss.jobTesterMetrics.IncrementTotalJobsFailed()
	ss.logger.ErrorContext(ctx, "job failed with non-success status",
		slog.Int("status_code", statusCode),
		slog.String("pipeline", stats.Pipeline),
		slog.String("model", stats.Model),
		slog.String("orchestrator", stats.Orchestrator))
	return ss.livepeerService.PostStats(ctx, stats)
}
