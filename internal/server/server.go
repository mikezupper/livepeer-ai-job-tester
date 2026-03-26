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
	"sync"
	"time"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/ffmpeg"
	status "livepeer-job-tester/internal/gateway/status"
	"livepeer-job-tester/internal/services"
	"livepeer-job-tester/internal/types"
)

const (
	startupGatewayFetchAttempts = 15
	startupGatewayFetchDelay    = 2 * time.Second
)

// ServerService defines the interface for starting the server and sending test jobs.
// It abstracts the operations needed to interact with orchestrators and pipelines.
type ServerService interface {
	StartServer(ctx context.Context, addr string) error
	SendTestJob(ctx context.Context, orchEthAddr, orchServiceUri, pipeline, model string, modelIsWarm bool) error
}

// EmbeddedWebhookServer represents the server responsible for managing job testing and orchestrator interactions.
// It contains configuration, a client, orchestrators, and a metrics service for tracking job test results.
type EmbeddedWebhookServer struct {
	lock                  sync.RWMutex               // Mutex to manage concurrent access to orchestrator data.
	config                *config.Config             // Configuration for the server, including API endpoints and credentials.
	livepeerService       services.LivepeerService   // Service to interact with Livepeer API for fetching orchestrators and pipelines.
	client                *http.Client               // HTTP client for making requests.
	orchestrators         []types.Orchestrator       // List of orchestrators fetched from the Livepeer API.
	orchToTest            string                     // Currently selected orchestrator for testing.
	jobTesterMetrics      *services.JobTesterMetrics // Metrics service for tracking job tester results.
	ffmpegClient          ffmpeg.Client
	statusClient          *status.Client
	liveManualAttachDelay time.Duration
	logger                *slog.Logger
}

// RuntimeOptions controls local/debug runtime behavior that should not be baked into shared config files.
type RuntimeOptions struct {
	LiveManualAttachDelay time.Duration
}

// NewEmbeddedWebhookServer creates a new instance of EmbeddedWebhookServer with the provided configuration, HTTP client, and Livepeer service.
// It initializes the server with empty orchestrator data and a new JobTesterMetrics instance.
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
		logger:                logger,
		jobTesterMetrics:      services.NewJobTesterMetrics(),
	}, nil
}

// StartServer starts the HTTP server and listens on the specified address.
// It sets up the web server handlers and manages the shutdown process.
func (ss *EmbeddedWebhookServer) StartServer(ctx context.Context, addr string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	mux := ss.webServerHandlers()
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ss.logger.InfoContext(ctx, "shutting down web server")
		if err := srv.Shutdown(shutdownCtx); err != nil {
			ss.logger.ErrorContext(ctx, "failed to shutdown web server", slog.Any("error", err))
		}
	}()

	ss.logger.InfoContext(ctx, "web server listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen and serve error: %w", err)
	}
	return nil
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
	orchestrators, err := ss.fetchOrchestratorsWithRetry(ctx)
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("failed to fetch orchestrators: %w", err)
	}
	ss.logger.InfoContext(ctx, "orchestrators fetched", slog.Int("count", len(orchestrators)))
	//make sure to update the orchestrators in a thread-safe manner
	//as it could be read by the web server handlers from the gateway
	ss.lock.Lock()
	ss.orchestrators = orchestrators
	ss.lock.Unlock()

	// Fetch pipelines
	pipelines, err := ss.fetchPipelinesWithRetry(ctx)
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("failed to fetch pipelines: %w", err)
	}

	orchestratorMap := make(map[string]types.OrchestratorCapability)
	for _, orchestrator := range pipelines.Orchestrators {
		orchestratorMap[orchestrator.Address] = orchestrator
		ss.logger.DebugContext(ctx, "capabilities loaded",
			slog.String("orchestrator", orchestrator.Address),
			slog.Int("pipelines", len(orchestrator.Pipelines)))
	}

	standardJobs, liveBundles := ss.buildExecutionPlan(ctx, orchestrators, orchestratorMap)
	ss.logger.InfoContext(ctx, "expected jobs computed", slog.Int("total", ss.jobTesterMetrics.ExpectedTotalJobs))

	for _, job := range standardJobs {
		ss.SetOrchToTest(job.ServiceURI)
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

func (ss *EmbeddedWebhookServer) fetchOrchestratorsWithRetry(ctx context.Context) ([]types.Orchestrator, error) {
	var lastErr error
	for attempt := 1; attempt <= startupGatewayFetchAttempts; attempt++ {
		orchestrators, err := ss.livepeerService.FetchOrchestrators(ctx)
		if err == nil {
			if attempt > 1 {
				ss.logger.InfoContext(ctx, "gateway orchestrator endpoint became ready",
					slog.Int("attempt", attempt))
			}
			return orchestrators, nil
		}

		lastErr = err
		if attempt == startupGatewayFetchAttempts {
			break
		}

		ss.logger.WarnContext(ctx, "gateway orchestrator endpoint not ready yet",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", startupGatewayFetchAttempts),
			slog.Duration("retry_delay", startupGatewayFetchDelay),
			slog.Any("error", err))

		if err := sleepWithContext(ctx, startupGatewayFetchDelay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (ss *EmbeddedWebhookServer) fetchPipelinesWithRetry(ctx context.Context) (*types.Pipelines, error) {
	var lastErr error
	for attempt := 1; attempt <= startupGatewayFetchAttempts; attempt++ {
		pipelines, err := ss.livepeerService.FetchPipelines(ctx)
		if err == nil {
			if attempt > 1 {
				ss.logger.InfoContext(ctx, "gateway capability endpoint became ready",
					slog.Int("attempt", attempt))
			}
			return pipelines, nil
		}

		lastErr = err
		if attempt == startupGatewayFetchAttempts {
			break
		}

		ss.logger.WarnContext(ctx, "gateway capability endpoint not ready yet",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", startupGatewayFetchAttempts),
			slog.Duration("retry_delay", startupGatewayFetchDelay),
			slog.Any("error", err))

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
func (ss *EmbeddedWebhookServer) buildExecutionPlan(ctx context.Context, orchestrators []types.Orchestrator, orchestratorMap map[string]types.OrchestratorCapability) ([]standardJob, []liveBundle) {
	var standardJobs []standardJob
	var liveBundles []liveBundle

	for _, orch := range orchestrators {
		capability, exists := orchestratorMap[orch.Address]
		if !exists {
			ss.logger.WarnContext(ctx, "orchestrator missing capability definition", slog.String("orchestrator", orch.Address))
			continue
		}

		for _, pipeline := range capability.Pipelines {
			pipelineName := pipeline.Type
			urisToTest, overrideApplied := ss.resolveServiceURIs(orch, pipelineName)
			if len(urisToTest) == 0 {
				ss.logger.WarnContext(ctx, "no service URIs resolved for orchestrator pipeline",
					slog.String("orchestrator", orch.Address),
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
							slog.String("orchestrator", orch.Address),
							slog.String("service_uri", uri),
							slog.String("pipeline", pipelineName))
					}

					if cfgPipeline.Live && pipelineName == "live-video-to-video" {
						bundle := liveBundle{
							OrchestratorAddress: orch.Address,
							ServiceURI:          uri,
							PipelineName:        pipelineName,
							Model:               model,
							Prompts:             make([]livePromptJob, 0, len(cfgPipeline.PromptVariants)),
						}
						for _, variant := range cfgPipeline.PromptVariants {
							ss.jobTesterMetrics.IncrementExpectedTotalJobs()
							bundle.Prompts = append(bundle.Prompts, livePromptJob{
								Variant:     variant,
								ModelIsWarm: model.Status.Warm > 0,
							})
							ss.logger.DebugContext(ctx, "queued live prompt job",
								slog.String("orchestrator", orch.Address),
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
						OrchestratorAddress: orch.Address,
						ServiceURI:          uri,
						PipelineName:        pipelineName,
						Model:               model,
					})
					ss.logger.DebugContext(ctx, "queued test job",
						slog.String("orchestrator", orch.Address),
						slog.String("service_uri", uri),
						slog.String("pipeline", pipelineName),
						slog.String("model", model.Name))
				}
			}
		}
	}

	return standardJobs, liveBundles
}

func (ss *EmbeddedWebhookServer) lookupLiveVideoOverrides(orchestratorAddr string) []string {
	liveCfg := ss.config.LiveVideo
	if liveCfg == nil || liveCfg.OrchMapping == nil {
		return nil
	}

	for mappedEthAddr, mappedURIs := range liveCfg.OrchMapping {
		if !strings.EqualFold(strings.ToLower(mappedEthAddr), strings.ToLower(orchestratorAddr)) {
			continue
		}

		var overrides []string
		for _, uri := range mappedURIs {
			trimmed := strings.TrimSpace(uri)
			if trimmed == "" {
				continue
			}
			overrides = append(overrides, strings.ToLower(trimmed))
		}

		if len(overrides) > 0 {
			return overrides
		}
		break
	}

	return nil
}

func (ss *EmbeddedWebhookServer) resolveServiceURIs(orchestrator types.Orchestrator, pipelineName string) ([]string, bool) {
	if cfg, ok := ss.findParametersByPipelineName(pipelineName); ok && cfg.Live {
		overrides := ss.lookupLiveVideoOverrides(orchestrator.Address)
		if len(overrides) > 0 {
			return overrides, true
		}
	}

	uri := strings.TrimSpace(orchestrator.ServiceURI)
	if uri == "" {
		return nil, false
	}

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
	copiedParams["orchestrator"] = orchServiceUri

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
		url := fmt.Sprintf("%s/%s?orchestrator=%s", ss.config.BroadcasterJobEndpoint, cfgPipeline.Uri, orchServiceUri)
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

// webServerHandlers sets up the HTTP handlers for the server, including the /orchestrators endpoint.
func (ss *EmbeddedWebhookServer) webServerHandlers() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/orchestrators", ss.handleOrchestrators)
	return mux
}

// handleOrchestrators handles HTTP GET requests to the /orchestrators endpoint.
// It returns a list of orchestrators in JSON format.
func (ss *EmbeddedWebhookServer) handleOrchestrators(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	type orch struct {
		Address string `json:"address"`
	}

	var orchs []orch
	orchToTest := ss.GetOrchToTest()
	if orchToTest == "" {
		// get a read lock to access the orchestrators slice
		// as it could be updated by the job tester concurrently
		ss.lock.RLock()
		for _, o := range ss.orchestrators {
			orchs = append(orchs, orch{o.ServiceURI})
		}
		ss.lock.RUnlock()
	} else {
		orchs = []orch{{orchToTest}}
	}

	ss.logger.InfoContext(r.Context(), "returning orchestrators", slog.Int("count", len(orchs)))

	res, err := json.Marshal(orchs)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	w.Write(res)
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

// SetOrchToTest sets the orchestrator currently being tested.
func (ss *EmbeddedWebhookServer) SetOrchToTest(orchServiceUri string) {
	ss.lock.Lock()
	defer ss.lock.Unlock()
	ss.orchToTest = orchServiceUri
}

// GetOrchToTest retrieves the currently selected orchestrator for testing.
func (ss *EmbeddedWebhookServer) GetOrchToTest() string {
	ss.lock.RLock()
	defer ss.lock.RUnlock()
	return ss.orchToTest
}
