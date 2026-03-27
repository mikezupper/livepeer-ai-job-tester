package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/ffmpeg"
	status "livepeer-job-tester/internal/gateway/status"
	"livepeer-job-tester/internal/types"
)

const (
	liveRetryPasses = 3

	liveOutcomePassed                = "passed"
	liveOutcomeFailed                = "failed"
	liveOutcomeDeferredBusy          = "deferred_busy"
	liveOutcomeDeferredIndeterminate = "deferred_indeterminate"
	liveOutcomeUnscoredBusy          = "unscored_busy"
	liveOutcomeUnscoredIndeterminate = "unscored_indeterminate"

	promptVerificationConfirmed  = "confirmed"
	promptVerificationUnverified = "unverified"
)

type standardJob struct {
	OrchestratorAddress string
	ServiceURI          string
	PipelineName        string
	Model               types.Model
}

type livePromptJob struct {
	Variant       config.PromptVariant
	ModelIsWarm   bool
	DeferAttempts int
}

type liveBundle struct {
	OrchestratorAddress string
	ServiceURI          string
	PipelineName        string
	// CapabilityModel is the value advertised by orchestrator capability discovery.
	// For live runs it acts as the gateway/worker selector, even though the
	// upstream capability schema names it a "model".
	CapabilityModel types.Model
	Prompts         []livePromptJob
}

type liveRunSpec struct {
	OrchestratorAddress string
	ServiceURI          string
	PipelineName        string
	// CapabilityModelName is the capability-advertised selector forwarded as the
	// top-level live pipeline/model identifier. It is distinct from any optional
	// nested params.model_id override passed to the runner.
	CapabilityModelName string
	ModelIsWarm         bool
	PromptID            string
	PromptComplexity    string
	ParamsJSON          string
	ParamsHash          string
	StreamID            string
	IngestURL           string
	PlaybackURL         string
	StatusEndpoint      string
}

type liveRunResult struct {
	Outcome             string
	PromptVerification  string
	Metrics             *ffmpeg.Metrics
	FirstMatchingStatus *status.LiveStatusSnapshot
	TerminalStatus      *status.LiveStatusSnapshot
	RunErr              error
	StreamValid         bool
}

// runLiveBundles executes live-video-to-video tests in orchestrator bundles so a busy
// orch can be deferred without burning the rest of that orch's prompt set immediately.
func (ss *EmbeddedWebhookServer) runLiveBundles(ctx context.Context, bundles []liveBundle) error {
	pending := bundles
	retryStates := make(map[string]liveBundleRetryState)
	for pass := 1; pass <= liveRetryPasses && len(pending) > 0; pass++ {
		pipelines, err := ss.livepeerService.FetchPipelines(ctx)
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return fmt.Errorf("refresh live capabilities: %w", err)
		}
		capabilityMap := capabilityMapByOrchestrator(pipelines)

		var nextPass []liveBundle
		processedThisPass := 0
		for _, bundle := range pending {
			retryKey := bundleRetryKey(bundle)
			if state, ok := retryStates[retryKey]; ok && time.Now().Before(state.CooldownUntil) {
				// A short per-run cooldown prevents tiny prompt sets from immediately
				// hammering the same orch/model again after it just reported saturation.
				ss.logger.InfoContext(ctx, "skipping live bundle during cooldown",
					slog.String("orchestrator", bundle.OrchestratorAddress),
					slog.String("service_uri", bundle.ServiceURI),
					slog.String("model", bundle.CapabilityModel.Name),
					slog.Time("cooldown_until", state.CooldownUntil),
					slog.String("reason", state.DeferredOutcome),
					slog.Int("pass", pass))
				nextPass = append(nextPass, bundle)
				continue
			}

			processedThisPass++
			remaining, retryState, err := ss.runLiveBundlePass(ctx, bundle, capabilityMap, pass)
			if err != nil {
				return err
			}
			if retryState != nil {
				retryStates[retryKey] = *retryState
			} else {
				delete(retryStates, retryKey)
			}
			if len(remaining.Prompts) > 0 {
				nextPass = append(nextPass, remaining)
			}
		}
		// If every remaining bundle is still cooling down, stop this invocation
		// instead of busy-looping through immediate retries with no new signal.
		if len(nextPass) > 0 && processedThisPass == 0 {
			return ss.finalizeCooldownPendingBundles(ctx, nextPass, retryStates)
		}
		pending = nextPass
	}
	if len(pending) > 0 {
		return ss.finalizeCooldownPendingBundles(ctx, pending, retryStates)
	}
	return nil
}

func (ss *EmbeddedWebhookServer) runLiveBundlePass(ctx context.Context, bundle liveBundle, capabilityMap map[string]types.OrchestratorCapability, pass int) (liveBundle, *liveBundleRetryState, error) {
	capabilityModel, exists := lookupCapabilityModel(capabilityMap, bundle.OrchestratorAddress, bundle.PipelineName, bundle.CapabilityModel.Name)
	if deferredOutcome, finalOutcome := deferOutcomesForCapability(exists, capabilityModel); deferredOutcome != "" {
		remaining, err := ss.deferOrFinalizeBundle(ctx, bundle, pass, deferredOutcome, finalOutcome)
		return remaining, newLiveBundleRetryState(time.Now(), ss.resolvedLiveRetryPolicy(), deferredOutcome, finalOutcome), err
	}

	for idx, promptJob := range bundle.Prompts {
		spec, err := ss.buildLiveRunSpec(bundle, promptJob)
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return liveBundle{}, nil, fmt.Errorf("build live run spec: %w", err)
		}

		ss.SetOrchToTest(bundle.ServiceURI)
		ss.logger.InfoContext(ctx, "running live prompt test",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.String("prompt_id", spec.PromptID),
			slog.String("prompt_complexity", spec.PromptComplexity),
			slog.String("stream_id", spec.StreamID),
			slog.String("ingest_url", spec.IngestURL),
			slog.String("playback_url", spec.PlaybackURL),
			slog.Int("pass", pass))

		spec, result, refreshedCapabilityModel, err := ss.executeLiveRunWithStartupRetry(ctx, bundle, promptJob, spec, capabilityModel)
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return liveBundle{}, nil, err
		}
		capabilityModel = refreshedCapabilityModel
		result, retryState := ss.reclassifyLiveFailure(ctx, bundle, result)
		switch result.Outcome {
		case liveOutcomeDeferredBusy:
			remaining, err := ss.deferRemainingPrompts(ctx, bundle, idx, pass, liveOutcomeDeferredBusy, liveOutcomeUnscoredBusy)
			return remaining, retryState, err
		case liveOutcomeDeferredIndeterminate:
			remaining, err := ss.deferRemainingPrompts(ctx, bundle, idx, pass, liveOutcomeDeferredIndeterminate, liveOutcomeUnscoredIndeterminate)
			return remaining, retryState, err
		default:
			stats := ss.buildLiveStats(spec, promptJob.DeferAttempts, result)
			ss.jobTesterMetrics.IncrementTotalJobs()
			if result.Outcome == liveOutcomePassed {
				if err := ss.handleSuccess(ctx, stats); err != nil {
					return liveBundle{}, nil, err
				}
			} else {
				if err := ss.handleRequestError(ctx, result.RunErr, "failed to execute live video stream", stats); err != nil {
					return liveBundle{}, nil, err
				}
			}
		}

		if idx == len(bundle.Prompts)-1 {
			continue
		}

		// Multi-prompt bundles reuse the same orch/model. We wait briefly for the
		// remote runner to recover capacity before sending the next prompt, then
		// defer the rest of the bundle if it still looks saturated.
		nextCapabilityModel, nextRetryState, err := ss.waitForNextLivePromptReadiness(ctx, bundle)
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return liveBundle{}, nil, err
		}
		if nextRetryState != nil {
			remaining, err := ss.deferRemainingPrompts(ctx, bundle, idx+1, pass, nextRetryState.DeferredOutcome, nextRetryState.FinalOutcome)
			return remaining, nextRetryState, err
		}
		capabilityModel = nextCapabilityModel
	}

	return liveBundle{}, nil, nil
}

func (ss *EmbeddedWebhookServer) buildLiveRunSpec(bundle liveBundle, promptJob livePromptJob) (liveRunSpec, error) {
	mergedParams := mergeLiveParams(ss.mustFindPipeline(bundle.PipelineName).Parameters, promptJob.Variant.Parameters)
	paramsJSON, paramsHash, err := canonicalizeLiveParams(mergedParams)
	if err != nil {
		return liveRunSpec{}, err
	}

	streamID := generatePromptAwareStreamID(promptJob.Variant.ID, promptJob.Variant.Complexity, paramsHash)
	liveCfg := ss.config.LiveVideo
	ingestURL, playbackURL, err := resolveLiveVideoURLs(liveCfg, streamID, bundle.CapabilityModel.Name, bundle.ServiceURI, paramsJSON)
	if err != nil {
		return liveRunSpec{}, err
	}

	return liveRunSpec{
		OrchestratorAddress: bundle.OrchestratorAddress,
		ServiceURI:          bundle.ServiceURI,
		PipelineName:        bundle.PipelineName,
		CapabilityModelName: bundle.CapabilityModel.Name,
		ModelIsWarm:         promptJob.ModelIsWarm,
		PromptID:            promptJob.Variant.ID,
		PromptComplexity:    strings.ToLower(promptJob.Variant.Complexity),
		ParamsJSON:          paramsJSON,
		ParamsHash:          paramsHash,
		StreamID:            streamID,
		IngestURL:           ingestURL,
		PlaybackURL:         playbackURL,
		StatusEndpoint:      buildLiveStatusEndpoint(ss.config.BroadcasterJobEndpoint, streamID),
	}, nil
}

func (ss *EmbeddedWebhookServer) executeLiveRun(ctx context.Context, spec liveRunSpec, capacityModel types.Model) liveRunResult {
	liveCfg := ss.config.LiveVideo
	videoPath := strings.TrimSpace(liveCfg.TestVideoPath)
	if videoPath == "" {
		videoPath = "test-assets/live-test-video.mp4"
	}

	streamResult, runErr := ss.ffmpegClient.RunStream(ctx, spec.IngestURL, spec.PlaybackURL, videoPath, buildStreamOptions(spec, liveCfg, ss.liveManualAttachDelay))
	var metrics *ffmpeg.Metrics
	if streamResult != nil {
		metrics = streamResult.Metrics
	}

	firstMatch, terminal := ss.captureStatusSnapshots(ctx, spec.StatusEndpoint, spec.ParamsHash)
	return buildLiveRunResult(metrics, runErr, firstMatch, terminal, capacityModel)
}

// buildStreamOptions keeps the live execution wiring in one place so the main
// run loop stays focused on orchestration and outcome handling.
func buildStreamOptions(spec liveRunSpec, liveCfg *config.LiveVideoConfig, manualAttachDelay time.Duration) ffmpeg.StreamOptions {
	return ffmpeg.StreamOptions{
		StatusEndpoint:     spec.StatusEndpoint,
		StatusPollInterval: time.Duration(liveCfg.StatusPollIntervalSeconds) * time.Second,
		StatusPollTimeout:  time.Duration(liveCfg.StatusPollTimeoutSeconds) * time.Second,
		TestDuration:       time.Duration(liveCfg.TestDurationSeconds) * time.Second,
		MetricRetryDelay:   time.Duration(liveCfg.MetricRetryDelayMilliseconds) * time.Millisecond,
		MaxMetricAttempts:  liveCfg.MaxMetricAttempts,
		MaxProbeAttempts:   liveCfg.MaxProbeAttempts,
		ManualAttachDelay:  manualAttachDelay,
	}
}

// buildLiveRunResult centralizes the live outcome classification.
func buildLiveRunResult(metrics *ffmpeg.Metrics, runErr error, firstMatch, terminal *status.LiveStatusSnapshot, capacityModel types.Model) liveRunResult {
	result := liveRunResult{
		Outcome:             liveOutcomePassed,
		PromptVerification:  promptVerificationUnverified,
		Metrics:             metrics,
		FirstMatchingStatus: firstMatch,
		TerminalStatus:      terminal,
		RunErr:              runErr,
		StreamValid:         runErr == nil,
	}
	if firstMatch != nil {
		result.PromptVerification = promptVerificationConfirmed
	}
	if runErr != nil {
		switch classifyRuntimeDeferReason(terminal, capacityModel) {
		case liveOutcomeDeferredBusy:
			result.Outcome = liveOutcomeDeferredBusy
		case liveOutcomeDeferredIndeterminate:
			result.Outcome = liveOutcomeDeferredIndeterminate
		default:
			result.Outcome = liveOutcomeFailed
		}
	}
	return result
}

func (ss *EmbeddedWebhookServer) captureStatusSnapshots(ctx context.Context, statusEndpoint, expectedHash string) (*status.LiveStatusSnapshot, *status.LiveStatusSnapshot) {
	var firstMatch *status.LiveStatusSnapshot
	var terminal *status.LiveStatusSnapshot

	// The worker emits full params inside periodic status events, not on every frame.
	// We poll a few times after the stream ends so the tester can confirm the prompt
	// via last_params_hash without keeping the live run open longer than necessary.
	for attempt := 0; attempt < 5; attempt++ {
		snapshot, _, err := ss.statusClient.Fetch(ctx, statusEndpoint)
		if err != nil {
			ss.logger.DebugContext(ctx, "failed to fetch live status snapshot", slog.Any("error", err))
		} else if snapshot != nil {
			terminal = snapshot
			if firstMatch == nil && snapshot.InferenceStatus.LastParamsHash == expectedHash && expectedHash != "" {
				firstMatch = snapshot
			}
		}
		if attempt < 4 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return firstMatch, terminal
}

func (ss *EmbeddedWebhookServer) deferRemainingPrompts(ctx context.Context, bundle liveBundle, startIdx, pass int, deferredOutcome, finalOutcome string) (liveBundle, error) {
	remaining := liveBundle{
		OrchestratorAddress: bundle.OrchestratorAddress,
		ServiceURI:          bundle.ServiceURI,
		PipelineName:        bundle.PipelineName,
		CapabilityModel:     bundle.CapabilityModel,
	}
	remaining.Prompts = append(remaining.Prompts, bundle.Prompts[startIdx:]...)
	return ss.deferOrFinalizeBundle(ctx, remaining, pass, deferredOutcome, finalOutcome)
}

func (ss *EmbeddedWebhookServer) deferOrFinalizeBundle(ctx context.Context, bundle liveBundle, pass int, deferredOutcome, finalOutcome string) (liveBundle, error) {
	for idx := range bundle.Prompts {
		bundle.Prompts[idx].DeferAttempts++
	}

	if pass < liveRetryPasses {
		for range bundle.Prompts {
			if deferredOutcome == liveOutcomeDeferredBusy {
				ss.jobTesterMetrics.IncrementTotalJobsDeferredBusy()
			} else {
				ss.jobTesterMetrics.IncrementTotalJobsDeferredIndeterminate()
			}
		}
		ss.logger.InfoContext(ctx, "deferring live bundle",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.String("reason", deferredOutcome),
			slog.Int("retry_pass", pass))
		return bundle, nil
	}
	if err := ss.finalizeDeferredLiveBundle(ctx, bundle, finalOutcome); err != nil {
		return liveBundle{}, err
	}

	return liveBundle{}, nil
}

func (ss *EmbeddedWebhookServer) buildLiveStats(spec liveRunSpec, deferAttempts int, result liveRunResult) *types.Stats {
	stats := &types.Stats{
		Region:             ss.config.Region,
		Pipeline:           spec.PipelineName,
		Model:              spec.CapabilityModelName,
		ModelIsWarm:        spec.ModelIsWarm,
		Orchestrator:       spec.OrchestratorAddress,
		Timestamp:          time.Now().Unix(),
		Errors:             make([]types.Error, 0),
		InputParameters:    spec.ParamsJSON,
		TestOutcome:        result.Outcome,
		PromptID:           spec.PromptID,
		PromptComplexity:   spec.PromptComplexity,
		PromptVerification: result.PromptVerification,
		StreamID:           spec.StreamID,
		ParamsHash:         spec.ParamsHash,
		DeferAttempts:      deferAttempts,
	}

	promptConfirmed := result.PromptVerification == promptVerificationConfirmed
	stats.PromptConfirmed = &promptConfirmed
	streamValid := result.StreamValid
	stats.StreamValid = &streamValid

	if result.Metrics != nil {
		applyMetricsToStats(stats, result.Metrics, ss.config.LiveVideo)
	}
	stats.ResponsePayload = buildLiveResponsePayload(spec, result)
	return stats
}

func (ss *EmbeddedWebhookServer) buildUnscoredLiveStats(spec liveRunSpec, deferAttempts int, outcome string) *types.Stats {
	return &types.Stats{
		Region:             ss.config.Region,
		Pipeline:           spec.PipelineName,
		Model:              spec.CapabilityModelName,
		ModelIsWarm:        spec.ModelIsWarm,
		Orchestrator:       spec.OrchestratorAddress,
		Timestamp:          time.Now().Unix(),
		Errors:             make([]types.Error, 0),
		InputParameters:    spec.ParamsJSON,
		ResponsePayload:    buildUnscoredLivePayload(spec, outcome),
		TestOutcome:        outcome,
		UnscoredReason:     strings.TrimPrefix(outcome, "unscored_"),
		PromptID:           spec.PromptID,
		PromptComplexity:   spec.PromptComplexity,
		PromptVerification: promptVerificationUnverified,
		StreamID:           spec.StreamID,
		ParamsHash:         spec.ParamsHash,
		DeferAttempts:      deferAttempts,
		OmitScore:          true,
	}
}

func buildLiveResponsePayload(spec liveRunSpec, result liveRunResult) string {
	payload := map[string]interface{}{
		"message":             "Live Video Test Completed",
		"stream_id":           spec.StreamID,
		"status_endpoint":     spec.StatusEndpoint,
		"playback_url":        spec.PlaybackURL,
		"test_outcome":        result.Outcome,
		"prompt_verification": result.PromptVerification,
		"params_hash":         spec.ParamsHash,
		"prompt_id":           spec.PromptID,
		"prompt_complexity":   spec.PromptComplexity,
	}
	if result.Metrics != nil {
		payload["total_frames"] = result.Metrics.TotalFrames()
		payload["average_fps"] = result.Metrics.AverageFPS()
		payload["average_latency"] = result.Metrics.AverageLatency()
		payload["test_duration"] = result.Metrics.DurationSeconds()
		payload["initial_latency"] = result.Metrics.InitialLatency()
		payload["gateway_ready_secs"] = result.Metrics.GatewayReadySeconds()
	}
	if result.FirstMatchingStatus != nil && result.FirstMatchingStatus.RawJSON != "" {
		payload["first_matching_status"] = json.RawMessage(result.FirstMatchingStatus.RawJSON)
	}
	if result.TerminalStatus != nil && result.TerminalStatus.RawJSON != "" {
		payload["terminal_status"] = json.RawMessage(result.TerminalStatus.RawJSON)
	}
	if result.RunErr != nil {
		payload["error"] = result.RunErr.Error()
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func buildUnscoredLivePayload(spec liveRunSpec, outcome string) string {
	payload := map[string]interface{}{
		"message":           "Live Video Test Unscored",
		"stream_id":         spec.StreamID,
		"test_outcome":      outcome,
		"unscored_reason":   strings.TrimPrefix(outcome, "unscored_"),
		"prompt_id":         spec.PromptID,
		"prompt_complexity": spec.PromptComplexity,
		"params_hash":       spec.ParamsHash,
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

// classifyRuntimeDeferReason maps terminal live-path errors into defer outcomes.
// Explicit gateway signals like OrchestratorCapped/Busy are treated as busy,
// while "no orchestrators available" only defers when fresh capability data
// also shows no idle capacity for the pinned orch/model.
func classifyRuntimeDeferReason(snapshot *status.LiveStatusSnapshot, capabilityModel types.Model) string {
	if snapshot == nil {
		return ""
	}
	message := strings.ToLower(snapshot.ErrorMessage())
	switch {
	case message == "":
		return ""
	case strings.Contains(message, "orchestratorcapped"),
		strings.Contains(message, "orchestratorbusy"),
		strings.Contains(message, "insufficient capacity"):
		return liveOutcomeDeferredBusy
	case strings.Contains(message, "no orchestrators available"):
		// This message can also appear after the pinned orch was removed from the
		// gateway pool for other reasons. We only treat it as a capacity deferral
		// when a fresh capability snapshot corroborates the lack of idle capacity.
		if capabilityModel.IdleCapacity == 0 && capabilityModel.CapacityInUse > 0 {
			return liveOutcomeDeferredBusy
		}
		if capabilityModel.IdleCapacity == 0 {
			return liveOutcomeDeferredIndeterminate
		}
	}
	return ""
}

func capabilityMapByOrchestrator(pipelines *types.Pipelines) map[string]types.OrchestratorCapability {
	out := make(map[string]types.OrchestratorCapability)
	if pipelines == nil {
		return out
	}
	for _, orch := range pipelines.Orchestrators {
		out[strings.ToLower(strings.TrimSpace(orch.Address))] = orch
	}
	return out
}

func lookupCapabilityModel(capabilityMap map[string]types.OrchestratorCapability, orchAddress, pipelineName, modelName string) (types.Model, bool) {
	orch, exists := capabilityMap[strings.ToLower(strings.TrimSpace(orchAddress))]
	if !exists {
		return types.Model{}, false
	}
	for _, pipeline := range orch.Pipelines {
		if pipeline.Type != pipelineName {
			continue
		}
		for _, model := range pipeline.Models {
			if model.Name == modelName {
				return model, true
			}
		}
	}
	return types.Model{}, false
}

func mergeLiveParams(base, overrides map[string]interface{}) map[string]interface{} {
	params := make(map[string]interface{}, len(base)+len(overrides))
	for key, value := range base {
		params[key] = value
	}
	for key, value := range overrides {
		params[key] = value
	}
	return params
}

func canonicalizeLiveParams(params map[string]interface{}) (string, string, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return "", "", err
	}
	sum := md5.Sum(data)
	return string(data), hex.EncodeToString(sum[:]), nil
}

func resolveLiveVideoURLs(cfg *config.LiveVideoConfig, streamKey, pipelineModel, serviceURI, paramsJSON string) (string, string, error) {
	mediaServerURLTrimmed := strings.TrimSpace(cfg.MediaServerURL)
	if mediaServerURLTrimmed == "" {
		return "", "", errors.New("media server URL missing")
	}

	ingest, err := buildLiveIngestURL(mediaServerURLTrimmed, streamKey, pipelineModel, serviceURI, paramsJSON)
	if err != nil {
		return "", "", err
	}
	playback := mediaServerURLTrimmed + "/" + streamKey + "-out"
	return ingest, playback, nil
}

// buildLiveIngestURL keeps the Mediamtx handoff unchanged. The original RTMP
// query string is forwarded verbatim by MediaMTX as $MTX_QUERY, so the tester
// must encode the full live payload up front with gateway routing fields plus a
// single params=<json> field that go-livepeer later unpacks.
func buildLiveIngestURL(base, streamKey, pipelineModel, serviceURI, paramsJSON string) (string, error) {
	trimmed := strings.TrimSpace(base)
	if trimmed == "" {
		return "", errors.New("media server URL missing")
	}
	if strings.Contains(trimmed, "{streamKey}") {
		trimmed = strings.ReplaceAll(trimmed, "{streamKey}", streamKey)
	} else if strings.HasSuffix(trimmed, "/") {
		trimmed += streamKey
	} else {
		trimmed = fmt.Sprintf("%s/%s", trimmed, streamKey)
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("pipeline", pipelineModel)
	q.Set("streamId", streamKey)
	q.Set("orchestrator", serviceURI)
	q.Set("params", paramsJSON)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func generatePromptAwareStreamID(promptID, complexity, paramsHash string) string {
	slug := promptIDRegex.ReplaceAllString(strings.ToLower(strings.TrimSpace(promptID)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "prompt"
	}
	if len(slug) > 24 {
		slug = slug[:24]
	}
	hash := paramsHash
	if len(hash) > 8 {
		hash = hash[:8]
	}
	return fmt.Sprintf("aiJobTesterStream-%s-%s-%s-%d", strings.ToLower(strings.TrimSpace(complexity)), slug, hash, time.Now().UnixNano())
}

func (ss *EmbeddedWebhookServer) mustFindPipeline(pipelineName string) *config.Pipeline {
	pipeline, _ := ss.findParametersByPipelineName(pipelineName)
	return pipeline
}

var promptIDRegex = regexp.MustCompile(`[^a-z0-9]+`)
