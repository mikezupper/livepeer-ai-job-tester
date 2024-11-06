package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/services"
	"livepeer-job-tester/internal/types"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// ServerService defines the interface for starting the server and sending test jobs.
// It abstracts the operations needed to interact with orchestrators and pipelines.
type ServerService interface {
	StartServer(addr string) error
	SendTestJob(orchEthAddr, orchServiceUri, pipeline, model string, modelIsWarm bool) error
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
}

// NewEmbeddedWebhookServer creates a new instance of EmbeddedWebhookServer with the provided configuration, HTTP client, and Livepeer service.
// It initializes the server with empty orchestrator data and a new JobTesterMetrics instance.
func NewEmbeddedWebhookServer(
	config *config.Config,
	client *http.Client,
	livepeerService services.LivepeerService,
) *EmbeddedWebhookServer {
	return &EmbeddedWebhookServer{
		config:           config,
		client:           client,
		livepeerService:  livepeerService,
		jobTesterMetrics: services.NewJobTesterMetrics(),
	}
}

// StartServer starts the HTTP server and listens on the specified address.
// It sets up the web server handlers and manages the shutdown process.
func (ss *EmbeddedWebhookServer) StartServer(addr string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := ss.webServerHandlers()
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Start a goroutine to handle graceful shutdown.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		log.Println("[StartServer] Shutting down web server")
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[StartServer] failed to shutdown web server: %v\n", err)
		}
	}()

	log.Printf("[StartServer] Web server listening at %s\n", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("[StartServer] ListenAndServe error: %w", err)
	}
	return nil
}

// RunTestJobs fetches orchestrators and pipelines from the Livepeer API and sends test jobs to each orchestrator.
// It increments job metrics and generates a JSON report of the job tester results.
func (ss *EmbeddedWebhookServer) RunTestJobs() error {
	// Fetch orchestrators
	orchestrators, err := ss.livepeerService.FetchOrchestrators()
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("failed to fetch orchestrators: %w", err)
	}
	log.Println("[EmbeddedWebhookServer] Orchestrators Found ", len(orchestrators))
	ss.orchestrators = orchestrators

	// Fetch pipelines
	pipelines, err := ss.livepeerService.FetchPipelines()
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("failed to fetch pipelines: %w", err)
	}
	orchestratorMap := make(map[string]types.OrchestratorCapability)

	// Iterate over the Orchestrators slice and populate the map
	for _, orchestrator := range pipelines.Orchestrators {
		orchestratorMap[orchestrator.Address] = orchestrator
	}
	// Calculate the total number of expected jobs.
	for _, o := range orchestrators {
		ethAddress := o.Address
		if orchCapability, exists := orchestratorMap[ethAddress]; exists {
			for _, pipeline := range orchCapability.Pipelines {
				pipelineName := pipeline.Type
				for _, model := range pipeline.Models {
					modelName := model.Name
					log.Println("Total Expected jobs increment ", pipelineName, modelName)
					ss.jobTesterMetrics.IncrementExpectedTotalJobs()
				}
			}
		}
	}

	// Send test jobs to orchestrators
	for _, o := range orchestrators {
		ethAddress := o.Address
		serviceURI := o.ServiceURI
		if capability, exists := orchestratorMap[ethAddress]; exists {
			for _, pipeline := range capability.Pipelines {
				pipelineName := pipeline.Type
				for _, model := range pipeline.Models {
					modelName := model.Name
					warmStatus := model.Status.Warm > 0
					log.Printf("[EmbeddedWebhookServer] sending AI Test Region [%s] Orch: %s ServiceURI: %s  Pipeline: %v Model: %s Warm: %v\n", ss.config.Region, ethAddress, serviceURI, pipelineName, modelName, warmStatus)
					ss.SetOrchToTest(serviceURI)
					err := ss.SendTestJob(ethAddress, serviceURI, pipelineName, modelName, warmStatus)
					if err != nil {
						log.Printf("[EmbeddedWebhookServer] Failed sending test job. Region [%s] Orch: [%s] pipeline [%s] model [%s] - Err [%v]\n", ss.config.Region, ethAddress, pipelineName, modelName, err)
					}
				}
			}
		}
	}

	// Generate the JSON report
	statsJSON, err := json.Marshal(ss.jobTesterMetrics)
	if err != nil {
		log.Println("Error marshalling job stats to JSON:", err)
		return err
	}
	log.Println("Job Stats Report:")
	log.Println(string(statsJSON))
	return nil
}

// SendTestJob sends a test job to the specified orchestrator and pipeline, including the model name and warm status.
// It updates the job tester metrics and processes the response, handling errors and capturing response data.
func (ss *EmbeddedWebhookServer) SendTestJob(orchEthAddr, orchServiceUri, pipeline, model string, modelIsWarm bool) error {
	// Increment total jobs metric.
	ss.jobTesterMetrics.IncrementTotalJobs()

	// Find pipeline parameters from the config.
	cfgPipeline, found := ss.findParametersByPipelineName(pipeline)
	if !found {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("[SendTestJob] pipeline not found in configuration file: %s", pipeline)
	}

	// Copy pipeline parameters and add the model ID.
	copiedParams := make(map[string]interface{})
	for key, value := range cfgPipeline.Parameters {
		copiedParams[key] = value
	}
	copiedParams["model_id"] = model

	// Marshal the parameters into JSON format.
	input, err := json.Marshal(copiedParams)
	if err != nil {
		ss.jobTesterMetrics.IncrementTotalJobsTesterError()
		return fmt.Errorf("[SendTestJob] failed to create job parameters for pipeline %s: %w", pipeline, err)
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

	// Send the HTTP request.
	url := fmt.Sprintf("%s/%s", ss.config.BroadcasterJobEndpoint, cfgPipeline.Uri)
	var req *http.Request
	if cfgPipeline.ContentType == "application/json" {
		req, err = http.NewRequest("POST", url, bytes.NewBuffer(input))
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return fmt.Errorf("[SendTestJob] failed to create new HTTP request: %w", err)
		}
		req.Header.Set("Content-Type", cfgPipeline.ContentType)
		req.Header.Set("Authorization", "Bearer "+ss.config.BroadcasterRequestToken)
	} else {
		req, err = ss.createMultipartRequest(url, copiedParams, cfgPipeline.Uri)
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return fmt.Errorf("[SendTestJob] failed to create multipart request: %w", err)
		}
	}

	// Measure round-trip time.
	startTime := time.Now()
	res, err := ss.client.Do(req)
	jobTime := time.Now()

	// Handle request errors.
	if err != nil {
		stats.RoundTripTime = jobTime.Sub(startTime).Seconds()
		return ss.handleRequestError(err, "failed to process the job", &stats)
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	readBodyTime := time.Now()
	if err != nil {
		stats.RoundTripTime = readBodyTime.Sub(startTime).Seconds()
		return ss.handleRequestError(err, "failed to read response body", &stats)
	}
	stats.RoundTripTime = readBodyTime.Sub(startTime).Seconds()

	// Check status code and handle errors.
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		//capture the error response from gateway
		stats.ResponsePayload = string(body)
		return ss.handleStatusCodeError(res.StatusCode, string(body), &stats)
	}

	// Capture response if necessary.
	if cfgPipeline.CaptureResponse {
		stats.ResponsePayload = string(body)
	} else {
		stats.ResponsePayload = "{\"message\":\"(Job Tester) Capture Response Disabled\"}"
	}

	// Finalize stats and report success.
	return ss.handleSuccess(&stats)
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
		for _, o := range ss.orchestrators {
			orchs = append(orchs, orch{o.ServiceURI})
		}
	} else {
		orchs = []orch{{orchToTest}}
	}

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
func (ss *EmbeddedWebhookServer) handleRequestError(err error, message string, stats *types.Stats) error {
	newError := types.Error{
		ErrorCode: fmt.Errorf("%w", err).Error(),
		Message:   message,
		Count:     1,
	}
	stats.Errors = append(stats.Errors, newError)
	ss.jobTesterMetrics.IncrementTotalJobsFailed()
	return ss.livepeerService.PostStats(stats)
}

// handleSuccess handles successful completion of a test job by updating job stats and posting them to the Leaderboard API.
func (ss *EmbeddedWebhookServer) handleSuccess(stats *types.Stats) error {
	stats.SuccessRate = 1
	ss.jobTesterMetrics.IncrementTotalJobsPassed()
	return ss.livepeerService.PostStats(stats)
}

// handleStatusCodeError handles errors related to non-2xx status codes in HTTP responses.
// It updates job stats and posts the error data to the Leaderboard API.
func (ss *EmbeddedWebhookServer) handleStatusCodeError(statusCode int, message string, stats *types.Stats) error {
	newError := types.Error{
		ErrorCode: strconv.Itoa(statusCode),
		Message:   message,
		Count:     1,
	}
	stats.Errors = append(stats.Errors, newError)
	ss.jobTesterMetrics.IncrementTotalJobsFailed()
	return ss.livepeerService.PostStats(stats)
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
