package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"livepeer-job-tester/internal/types"
)

type liveRetryPolicy struct {
	InterPromptSettleDelay      time.Duration
	InterPromptReadyTimeout     time.Duration
	InterPromptPollInterval     time.Duration
	RetryCooldown               time.Duration
	StartupRetryAttempts        int
	StartupRetryBaseDelay       time.Duration
	StartupRetryTeardownTimeout time.Duration
}

type liveBundleRetryState struct {
	CooldownUntil   time.Time
	DeferredOutcome string
	FinalOutcome    string
}

var defaultLiveRetryPolicy = liveRetryPolicy{
	InterPromptSettleDelay:      10 * time.Second,
	InterPromptReadyTimeout:     10 * time.Second,
	InterPromptPollInterval:     1 * time.Second,
	RetryCooldown:               30 * time.Second,
	StartupRetryAttempts:        2,
	StartupRetryBaseDelay:       10 * time.Second,
	StartupRetryTeardownTimeout: 5 * time.Second,
}

func (ss *EmbeddedWebhookServer) resolvedLiveRetryPolicy() liveRetryPolicy {
	policy := ss.liveRetryPolicy
	if policy.InterPromptSettleDelay <= 0 {
		policy.InterPromptSettleDelay = defaultLiveRetryPolicy.InterPromptSettleDelay
	}
	if policy.InterPromptReadyTimeout <= 0 {
		policy.InterPromptReadyTimeout = defaultLiveRetryPolicy.InterPromptReadyTimeout
	}
	if policy.InterPromptPollInterval <= 0 {
		policy.InterPromptPollInterval = defaultLiveRetryPolicy.InterPromptPollInterval
	}
	if policy.RetryCooldown <= 0 {
		policy.RetryCooldown = defaultLiveRetryPolicy.RetryCooldown
	}
	if policy.StartupRetryAttempts <= 0 {
		policy.StartupRetryAttempts = defaultLiveRetryPolicy.StartupRetryAttempts
	}
	if policy.StartupRetryBaseDelay <= 0 {
		policy.StartupRetryBaseDelay = defaultLiveRetryPolicy.StartupRetryBaseDelay
	}
	if policy.StartupRetryTeardownTimeout <= 0 {
		policy.StartupRetryTeardownTimeout = defaultLiveRetryPolicy.StartupRetryTeardownTimeout
	}
	return policy
}

func bundleRetryKey(bundle liveBundle) string {
	return strings.ToLower(strings.Join([]string{
		strings.TrimSpace(bundle.OrchestratorAddress),
		strings.TrimSpace(bundle.ServiceURI),
		strings.TrimSpace(bundle.PipelineName),
		strings.TrimSpace(bundle.CapabilityModel.Name),
	}, "|"))
}

func newLiveBundleRetryState(now time.Time, policy liveRetryPolicy, deferredOutcome, finalOutcome string) *liveBundleRetryState {
	if deferredOutcome == "" || finalOutcome == "" {
		return nil
	}
	return &liveBundleRetryState{
		CooldownUntil:   now.Add(policy.RetryCooldown),
		DeferredOutcome: deferredOutcome,
		FinalOutcome:    finalOutcome,
	}
}

func deferOutcomesForCapability(exists bool, capabilityModel types.Model) (string, string) {
	if !exists {
		return liveOutcomeDeferredIndeterminate, liveOutcomeUnscoredIndeterminate
	}
	if capabilityModel.IdleCapacity > 0 {
		return "", ""
	}
	if capabilityModel.CapacityInUse > 0 {
		return liveOutcomeDeferredBusy, liveOutcomeUnscoredBusy
	}
	return liveOutcomeDeferredIndeterminate, liveOutcomeUnscoredIndeterminate
}

func finalOutcomeForDeferred(deferredOutcome string) string {
	switch deferredOutcome {
	case liveOutcomeDeferredBusy:
		return liveOutcomeUnscoredBusy
	case liveOutcomeDeferredIndeterminate:
		return liveOutcomeUnscoredIndeterminate
	default:
		return ""
	}
}

func (ss *EmbeddedWebhookServer) fetchLiveCapabilityModel(ctx context.Context, bundle liveBundle) (types.Model, bool, error) {
	pipelines, err := ss.livepeerService.FetchPipelines(ctx)
	if err != nil {
		return types.Model{}, false, err
	}
	model, exists := lookupCapabilityModel(
		capabilityMapByOrchestrator(pipelines),
		bundle.OrchestratorAddress,
		bundle.PipelineName,
		bundle.CapabilityModel.Name,
	)
	return model, exists, nil
}

// classifyLivePromptStartupRetryReason returns a non-empty reason only when a
// prompt failed before the stream ever became ready and the terminal signal
// still points to a capacity-style startup rejection. That keeps prompt-local
// retries narrowly scoped to the "start failed too early" case instead of
// retrying failures that happened after the stream was already online.
func classifyLivePromptStartupRetryReason(result liveRunResult) string {
	if result.RunErr == nil || result.Metrics != nil || result.TerminalStatus == nil {
		return ""
	}

	message := strings.TrimSpace(result.TerminalStatus.ErrorMessage())
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "orchestratorcapped"),
		strings.Contains(lower, "orchestratorbusy"),
		strings.Contains(lower, "insufficient capacity"),
		strings.Contains(lower, "no orchestrators available"):
		return message
	default:
		return ""
	}
}

func startupRetryDelay(policy liveRetryPolicy, retry int) time.Duration {
	if retry <= 0 || policy.StartupRetryBaseDelay <= 0 {
		return 0
	}
	return policy.StartupRetryBaseDelay * time.Duration(1<<(retry-1))
}

// classifyStartupRetryExhaustedOutcome prefers the gateway's repeated
// capacity-style startup rejection over an optimistic capability refresh. This
// avoids turning admission races into scored failures when the gateway has
// already proven the orch cannot accept the stream yet.
func classifyStartupRetryExhaustedOutcome(result liveRunResult) string {
	if classifyLivePromptStartupRetryReason(result) == "" {
		return ""
	}
	return liveOutcomeDeferredBusy
}

// executeLiveRunWithStartupRetry retries the same prompt when the gateway/orch
// rejects the live start before readiness is reached. This is separate from the
// inter-prompt settle/cooldown policy: the goal here is to recover a single
// prompt that lost a startup race without stretching every successful prompt
// transition by the same amount.
func (ss *EmbeddedWebhookServer) executeLiveRunWithStartupRetry(ctx context.Context, bundle liveBundle, promptJob livePromptJob, spec liveRunSpec, capabilityModel types.Model) (liveRunSpec, liveRunResult, types.Model, error) {
	return ss.executeLiveRunWithStartupRetryFunc(
		ctx,
		bundle,
		spec,
		capabilityModel,
		func(spec liveRunSpec, model types.Model) liveRunResult {
			return ss.executeLiveRun(ctx, spec, model)
		},
		func() (liveRunSpec, error) {
			return ss.buildLiveRunSpec(bundle, promptJob)
		},
	)
}

func (ss *EmbeddedWebhookServer) executeLiveRunWithStartupRetryFunc(
	ctx context.Context,
	bundle liveBundle,
	spec liveRunSpec,
	capabilityModel types.Model,
	run func(liveRunSpec, types.Model) liveRunResult,
	buildNextSpec func() (liveRunSpec, error),
) (liveRunSpec, liveRunResult, types.Model, error) {
	policy := ss.resolvedLiveRetryPolicy()
	result := run(spec, capabilityModel)

	for retry := 1; retry <= policy.StartupRetryAttempts; retry++ {
		reason := classifyLivePromptStartupRetryReason(result)
		if reason == "" {
			return spec, result, capabilityModel, nil
		}

		delay := startupRetryDelay(policy, retry)
		ss.logger.InfoContext(ctx, "retrying live prompt after early startup failure",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.String("prompt_id", spec.PromptID),
			slog.Int("retry", retry),
			slog.Int("max_retries", policy.StartupRetryAttempts),
			slog.Duration("backoff", delay),
			slog.String("reason", reason))

		if err := ss.waitForFailedLiveStreamTeardown(ctx, spec.StatusEndpoint, policy); err != nil {
			return spec, result, capabilityModel, err
		}
		if err := sleepWithContext(ctx, delay); err != nil {
			return spec, result, capabilityModel, err
		}

		refreshedModel, exists, err := ss.fetchLiveCapabilityModel(ctx, bundle)
		if err != nil {
			ss.logger.WarnContext(ctx, "failed to refresh live capabilities before startup retry",
				slog.String("orchestrator", bundle.OrchestratorAddress),
				slog.String("service_uri", bundle.ServiceURI),
				slog.String("model", bundle.CapabilityModel.Name),
				slog.String("prompt_id", spec.PromptID),
				slog.Any("error", err))
		} else if exists {
			capabilityModel = refreshedModel
		}

		nextSpec, err := buildNextSpec()
		if err != nil {
			return spec, result, capabilityModel, err
		}
		ss.logger.InfoContext(ctx, "restarting live prompt with fresh stream",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.String("prompt_id", nextSpec.PromptID),
			slog.String("stream_id", nextSpec.StreamID))
		spec = nextSpec
		result = run(spec, capabilityModel)
	}

	if exhaustedOutcome := classifyStartupRetryExhaustedOutcome(result); exhaustedOutcome != "" {
		result.Outcome = exhaustedOutcome
	}

	return spec, result, capabilityModel, nil
}

// waitForFailedLiveStreamTeardown gives the failed startup attempt a short
// bounded window to disappear before the next retry is issued with a fresh
// stream ID. The live gateway API does not expose a matching public stop call
// for this flow, so the tester uses the old status endpoint as its teardown
// barrier before starting the next attempt.
func (ss *EmbeddedWebhookServer) waitForFailedLiveStreamTeardown(ctx context.Context, statusEndpoint string, policy liveRetryPolicy) error {
	if ss.statusClient == nil || strings.TrimSpace(statusEndpoint) == "" || policy.StartupRetryTeardownTimeout <= 0 {
		return nil
	}

	deadline := time.Now().Add(policy.StartupRetryTeardownTimeout)
	for {
		snapshot, statusCode, err := ss.statusClient.Fetch(ctx, statusEndpoint)
		if err == nil {
			if statusCode == 404 {
				return nil
			}
			if snapshot != nil && strings.TrimSpace(snapshot.ErrorMessage()) != "" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		if err := sleepWithContext(ctx, policy.InterPromptPollInterval); err != nil {
			return err
		}
	}
}

// waitForNextLivePromptReadiness gives the same orch/model a short chance to
// free capacity after a successful prompt before the tester sends the next
// prompt in the bundle. If readiness does not return within the bounded window,
// the remaining prompts are deferred and the bundle enters a cooldown period.
func (ss *EmbeddedWebhookServer) waitForNextLivePromptReadiness(ctx context.Context, bundle liveBundle) (types.Model, *liveBundleRetryState, error) {
	policy := ss.resolvedLiveRetryPolicy()

	// The capability endpoint can briefly look optimistic while the gateway and
	// remote runner are still tearing down the previous live session. A small
	// fixed settle delay avoids re-entering the same orch/model immediately based
	// on stale "ready" data.
	if policy.InterPromptSettleDelay > 0 {
		ss.logger.InfoContext(ctx, "waiting for inter-prompt settle delay",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.Duration("delay", policy.InterPromptSettleDelay))
		select {
		case <-time.After(policy.InterPromptSettleDelay):
		case <-ctx.Done():
			return types.Model{}, nil, ctx.Err()
		}
	}

	deadline := time.Now().Add(policy.InterPromptReadyTimeout)

	for attempt := 1; ; attempt++ {
		capabilityModel, exists, err := ss.fetchLiveCapabilityModel(ctx, bundle)
		if err != nil {
			return types.Model{}, nil, fmt.Errorf("refresh live capabilities: %w", err)
		}

		deferredOutcome, finalOutcome := deferOutcomesForCapability(exists, capabilityModel)
		if deferredOutcome == "" {
			if attempt > 1 {
				ss.logger.InfoContext(ctx, "live bundle ready for next prompt",
					slog.String("orchestrator", bundle.OrchestratorAddress),
					slog.String("service_uri", bundle.ServiceURI),
					slog.String("model", bundle.CapabilityModel.Name),
					slog.Int("attempt", attempt))
			}
			return capabilityModel, nil, nil
		}

		if time.Now().After(deadline) {
			retryState := newLiveBundleRetryState(time.Now(), policy, deferredOutcome, finalOutcome)
			ss.logger.InfoContext(ctx, "live bundle readiness window expired",
				slog.String("orchestrator", bundle.OrchestratorAddress),
				slog.String("service_uri", bundle.ServiceURI),
				slog.String("model", bundle.CapabilityModel.Name),
				slog.String("reason", deferredOutcome),
				slog.Duration("cooldown", policy.RetryCooldown))
			return types.Model{}, retryState, nil
		}

		ss.logger.DebugContext(ctx, "waiting for live bundle readiness",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.String("reason", deferredOutcome),
			slog.Int("attempt", attempt))

		select {
		case <-time.After(policy.InterPromptPollInterval):
		case <-ctx.Done():
			return types.Model{}, nil, ctx.Err()
		}
	}
}

// reclassifyLiveFailure refreshes live capabilities once more after a failed
// prompt so transient cleanup/capacity transitions can be interpreted with
// current orch state instead of the stale snapshot fetched at bundle start.
func (ss *EmbeddedWebhookServer) reclassifyLiveFailure(ctx context.Context, bundle liveBundle, result liveRunResult) (liveRunResult, *liveBundleRetryState) {
	if result.Outcome != liveOutcomeFailed || result.RunErr == nil {
		return result, nil
	}

	capabilityModel, exists, err := ss.fetchLiveCapabilityModel(ctx, bundle)
	if err != nil {
		ss.logger.WarnContext(ctx, "failed to refresh live capabilities after runtime failure",
			slog.String("orchestrator", bundle.OrchestratorAddress),
			slog.String("service_uri", bundle.ServiceURI),
			slog.String("model", bundle.CapabilityModel.Name),
			slog.Any("error", err))
		return result, nil
	}
	if !exists {
		retryState := newLiveBundleRetryState(time.Now(), ss.resolvedLiveRetryPolicy(), liveOutcomeDeferredIndeterminate, liveOutcomeUnscoredIndeterminate)
		result.Outcome = liveOutcomeDeferredIndeterminate
		return result, retryState
	}

	deferredOutcome := classifyRuntimeDeferReason(result.TerminalStatus, capabilityModel)
	if deferredOutcome == "" {
		return result, nil
	}

	retryState := newLiveBundleRetryState(time.Now(), ss.resolvedLiveRetryPolicy(), deferredOutcome, finalOutcomeForDeferred(deferredOutcome))
	result.Outcome = deferredOutcome
	return result, retryState
}

func (ss *EmbeddedWebhookServer) finalizeDeferredLiveBundle(ctx context.Context, bundle liveBundle, finalOutcome string) error {
	for _, promptJob := range bundle.Prompts {
		spec, err := ss.buildLiveRunSpec(bundle, promptJob)
		if err != nil {
			ss.jobTesterMetrics.IncrementTotalJobsTesterError()
			return fmt.Errorf("build unscored live run spec: %w", err)
		}

		stats := ss.buildUnscoredLiveStats(spec, promptJob.DeferAttempts, finalOutcome)
		ss.jobTesterMetrics.IncrementTotalJobs()
		if finalOutcome == liveOutcomeUnscoredBusy {
			ss.jobTesterMetrics.IncrementTotalJobsUnscoredBusy()
		} else {
			ss.jobTesterMetrics.IncrementTotalJobsUnscoredIndeterminate()
		}
		if err := ss.livepeerService.PostStats(ctx, stats); err != nil {
			return err
		}
	}

	return nil
}

// finalizeCooldownPendingBundles stops the current run from immediately
// retrying bundles that are still cooling down. Those prompts have already been
// counted as deferred once; at this point they become unscored for the current
// invocation instead of spinning through more immediate retries.
func (ss *EmbeddedWebhookServer) finalizeCooldownPendingBundles(ctx context.Context, bundles []liveBundle, retryStates map[string]liveBundleRetryState) error {
	for _, bundle := range bundles {
		state, ok := retryStates[bundleRetryKey(bundle)]
		finalOutcome := liveOutcomeUnscoredIndeterminate
		if ok && state.FinalOutcome != "" {
			finalOutcome = state.FinalOutcome
		}
		if err := ss.finalizeDeferredLiveBundle(ctx, bundle, finalOutcome); err != nil {
			return err
		}
	}
	return nil
}
