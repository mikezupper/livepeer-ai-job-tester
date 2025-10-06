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
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/ffmpeg"
	"livepeer-job-tester/internal/services"
	"livepeer-job-tester/internal/types"
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
	lock             sync.RWMutex               // Mutex to manage concurrent access to orchestrator data.
	config           *config.Config             // Configuration for the server, including API endpoints and credentials.
	livepeerService  services.LivepeerService   // Service to interact with Livepeer API for fetching orchestrators and pipelines.
	client           *http.Client               // HTTP client for making requests.
	orchestrators    []types.Orchestrator       // List of orchestrators fetched from the Livepeer API.
	orchToTest       string                     // Currently selected orchestrator for testing.
	jobTesterMetrics *services.JobTesterMetrics // Metrics service for tracking job tester results.
	ffmpegClient     ffmpeg.Client
	logger           *slog.Logger
}

// NewEmbeddedWebhookServer creates a new instance of EmbeddedWebhookServer with the provided configuration, HTTP client, and Livepeer service.
// It initializes the server with empty orchestrator data and a new JobTesterMetrics instance.
func NewEmbeddedWebhookServer(
	config *config.Config,
	client *http.Client,
	livepeerService services.LivepeerService,
	ffmpegClient ffmpeg.Client,
	logger *slog.Logger,
) *EmbeddedWebhookServer {
	if logger == nil {
		logger = slog.Default()
	}
	if ffmpegClient == nil {
		ffmpegClient = ffmpeg.NewClient(logger)
	}

	return &EmbeddedWebhookServer{
		config:           config,
		client:           client,
		livepeerService:  livepeerService,
		ffmpegClient:     ffmpegClient,
		logger:           logger,
		jobTesterMetrics: services.NewJobTesterMetrics(),
	}
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

	orchestrators, err := ss.livepeerService.FetchOrchestrators(ctx)
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
	pipelines, err := ss.livepeerService.FetchPipelines(ctx)
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

	for _, o := range orchestrators {
		ethAddress := o.Address
		capability, exists := orchestratorMap[ethAddress]
		if !exists {
			ss.logger.WarnContext(ctx, "orchestrator missing capability definition", slog.String("orchestrator", ethAddress))
			continue
		}

		for _, pipeline := range capability.Pipelines {
			pipelineName := pipeline.Type
			urisToTest, overrideApplied := ss.resolveServiceURIs(o, pipelineName)
			if len(urisToTest) == 0 {
				ss.logger.WarnContext(ctx, "no service URIs resolved for orchestrator pipeline",
					slog.String("orchestrator", ethAddress),
					slog.String("pipeline", pipelineName))
				continue
			}

			for _, model := range pipeline.Models {
				for _, uri := range urisToTest {
					ss.jobTesterMetrics.IncrementExpectedTotalJobs()
					ss.logger.DebugContext(ctx, "queued test job",
						slog.String("orchestrator", ethAddress),
						slog.String("service_uri", uri),
						slog.String("pipeline", pipelineName),
						slog.String("model", model.Name),
						slog.Bool("override", overrideApplied))
				}
			}
		}
	}

	ss.logger.InfoContext(ctx, "expected jobs computed", slog.Int("total", ss.jobTesterMetrics.ExpectedTotalJobs))

	for _, orchestrator := range orchestrators {
		ethAddress := orchestrator.Address
		capability, exists := orchestratorMap[ethAddress]
		if !exists {
			continue
		}

		for _, pipeline := range capability.Pipelines {
			pipelineName := pipeline.Type
			urisToTest, overrideApplied := ss.resolveServiceURIs(orchestrator, pipelineName)
			if len(urisToTest) == 0 {
				continue
			}

			for _, model := range pipeline.Models {
				modelName := model.Name
				warmStatus := model.Status.Warm > 0

				for _, serviceURI := range urisToTest {
					if overrideApplied {
						ss.logger.DebugContext(ctx, "overriding service URI for orchestrator",
							slog.String("orchestrator", ethAddress),
							slog.String("service_uri", serviceURI),
							slog.String("pipeline", pipelineName))
					}

					ss.SetOrchToTest(serviceURI)
					ss.logger.InfoContext(ctx, "sending test job",
						slog.String("region", ss.config.Region),
						slog.String("orchestrator", ethAddress),
						slog.String("service_uri", serviceURI),
						slog.String("pipeline", pipelineName),
						slog.String("model", modelName),
						slog.Bool("warm", warmStatus))

					if err := ss.SendTestJob(ctx, ethAddress, serviceURI, pipelineName, modelName, warmStatus); err != nil {
						ss.logger.ErrorContext(ctx, "failed to send test job",
							slog.String("region", ss.config.Region),
							slog.String("orchestrator", ethAddress),
							slog.String("service_uri", serviceURI),
							slog.String("pipeline", pipelineName),
							slog.String("model", modelName),
							slog.Any("error", err))
					}
				}
			}
		}
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

	// Copy pipeline parameters and add the model ID.
	copiedParams := make(map[string]any)
	for key, value := range cfgPipeline.Parameters {
		copiedParams[key] = value
	}
	copiedParams["model_id"] = model

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
		if err := ss.handleLiveVideoTest(ctx, &stats, copiedParams); err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			ss.logger.ErrorContext(ctx, "live video test failed", slog.Any("error", err))
			return err
		}
		return ss.handleSuccess(ctx, &stats)
	}

	// Send the HTTP request
	url := fmt.Sprintf("%s/%s", ss.config.BroadcasterJobEndpoint, cfgPipeline.Uri)
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

func (ss *EmbeddedWebhookServer) handleLiveVideoTest(ctx context.Context, stats *types.Stats, params map[string]any) error {
	if ctx == nil {
		ctx = context.Background()
	}

	startTime := time.Now()

	liveCfg := ss.config.LiveVideo
	if liveCfg == nil {
		stats.RoundTripTime = time.Since(startTime).Seconds()
		return ss.handleRequestError(ctx, errors.New("live video configuration missing"), "live video configuration not provided", stats)
	}
	videoPath := strings.TrimSpace(liveCfg.TestVideoPath)
	if videoPath == "" {
		videoPath = "test-assets/live-test-video.mp4"
	}

	// Live video stream key used for the test
	const streamKey = "aiJobTesterStream"
	ingestURL, playbackURL, err := ss.resolveLiveVideoURLs(liveCfg, streamKey, params)
	if err != nil {
		stats.RoundTripTime = time.Since(startTime).Seconds()
		return ss.handleRequestError(ctx, err, "invalid live video configuration", stats)
	}

	ss.logger.InfoContext(ctx, "running live video stream",
		slog.String("ingest_url", ingestURL),
		slog.String("playback_url", playbackURL))

	metrics, err := ss.ffmpegClient.RunStream(ctx, ingestURL, playbackURL, videoPath, ffmpeg.StreamOptions{
		GracePeriod:  time.Duration(liveCfg.ProbeGracePeriodSeconds) * time.Second,
		TestDuration: time.Duration(liveCfg.TestDurationSeconds) * time.Second,
	})
	if err != nil {
		stats.RoundTripTime = time.Since(startTime).Seconds()
		return ss.handleRequestError(ctx, err, "failed to execute live video stream", stats)
	}

	stats.RoundTripTime = time.Since(startTime).Seconds()
	stats.AverageFPS = metrics.AverageFPS()
	stats.AverageLatency = metrics.AverageLatency()
	stats.TotalFrames = metrics.TotalFrames()
	stats.TestDuration = metrics.DurationSeconds()
	stats.InitialLatency = metrics.InitialLatency()
	stats.StreamScore = metrics.Score(liveCfg.TargetFPS, liveCfg.MaxInitialLatencySeconds, liveCfg.ProbeGracePeriodSeconds)

	ss.logger.InfoContext(ctx, "live video test completed",
		slog.String("stream_key", streamKey),
		slog.Int("total_frames", metrics.TotalFrames()),
		slog.Float64("avg_fps", metrics.AverageFPS()),
		slog.Float64("avg_latency", metrics.AverageLatency()),
		slog.Float64("duration", metrics.DurationSeconds()),
		slog.Float64("initial_latency", metrics.InitialLatency()),
		slog.Float64("stream_score", stats.StreamScore))

	return nil
}

func (ss *EmbeddedWebhookServer) resolveLiveVideoURLs(cfg *config.LiveVideoConfig, streamKey string, params map[string]any) (string, string, error) {
	ingest := strings.TrimSpace(cfg.IngestURL)
	playback := strings.TrimSpace(cfg.PlaybackURL)
	if ingest == "" || playback == "" {
		return "", "", errors.New("rtmp ingest or playback URL missing")
	}

	ingest = buildStreamURL(ingest, streamKey, params)
	playback = playback + "/" + streamKey + "-out"
	return ingest, playback, nil
}

func buildStreamURL(base, streamKey string, params map[string]any) string {
	trimmed := strings.TrimSpace(base)
	if trimmed == "" {
		return trimmed
	}

	// Add streamKey to the URL
	if strings.Contains(trimmed, "{streamKey}") {
		trimmed = strings.ReplaceAll(trimmed, "{streamKey}", streamKey)
	} else if strings.HasSuffix(trimmed, "/") {
		trimmed += streamKey
	} else {
		trimmed = fmt.Sprintf("%s/%s", trimmed, streamKey)
	}

	// Add all params as individual query parameters
	if len(params) > 0 {
		u, err := url.Parse(trimmed)
		if err == nil {
			q := u.Query()
			for key, value := range params {
				q.Set(key, fmt.Sprintf("%v", value))
			}
			// Add stream key to the query as "streamId=<streamKey>"
			q.Set("streamId", streamKey)
			u.RawQuery = q.Encode()
			trimmed = u.String()
		}
	}

	return trimmed
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
		if pipeline.Name == pipelineName {
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
