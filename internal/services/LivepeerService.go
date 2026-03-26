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
	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/types"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
		orchestrator.Address = strings.ToLower(strings.TrimSpace(orchestrator.Address))
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
// - pipeline/model list comes from `hardware[]` (pipeline/model_id)
// - warm/capacity/runnerVersion comes from `capabilities.constraints.PerCapability[capID].models[model_id]`
// - logs GPU info (if available)
// - emits per-orchestrator summary (WARN if missing constraints)
// - emits per-orchestrator runnerVersion min/max (semver-ish)
// - emits global summary across all orchestrators
func (s *HTTPLivepeerService) FetchPipelines(ctx context.Context) (*types.Pipelines, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	url := fmt.Sprintf("%s/getNetworkCapabilities", s.config.BroadcasterCliEndpoint)
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

	var netCaps types.NetworkCapabilitiesResponse
	if err := json.Unmarshal(body, &netCaps); err != nil {
		return nil, err
	}

	// slugified capability name -> capability ID
	slugToCapID := map[string]string{}
	for capID, capName := range netCaps.CapabilityNames {
		slugToCapID[slugifyCapabilityName(capName)] = capID
	}

	// -------- Global summary counters --------
	totalOrchs := 0
	totalPipelines := 0
	totalModels := 0
	totalGPUs := 0
	totalMissingConstraints := 0
	orchsWithMissingConstraints := 0

	// Track distinct pipelines/models globally (nice-to-have)
	globalPipelineSet := map[string]bool{}
	globalModelSet := map[string]bool{}
	globalRunnerVersions := []string{}

	var pipelines types.Pipelines
	for _, orch := range netCaps.Orchestrators {
		totalOrchs++
		orchAddr := strings.ToLower(strings.TrimSpace(orch.Address))
		orchCap := types.OrchestratorCapability{Address: orchAddr}
		// -------- Per-orchestrator summary counters --------
		pipelineCount := 0
		modelCount := 0
		gpuCount := 0
		missingConstraintCount := 0

		// Track runner versions seen for this orch for min/max
		orchRunnerVersions := []string{}

		// Group models by pipeline (from hardware)
		pipelineByName := map[string]*types.Pipeline{}
		seenModel := map[string]map[string]bool{} // pipeline -> model -> seen

		for _, hw := range orch.Hardware {
			pName := hw.Pipeline
			globalPipelineSet[pName] = true

			p, exists := pipelineByName[pName]
			if !exists {
				p = &types.Pipeline{Type: pName}
				pipelineByName[pName] = p
				pipelineCount++
			}

			if _, ok := seenModel[pName]; !ok {
				seenModel[pName] = map[string]bool{}
			}
			alreadyAdded := seenModel[pName][hw.ModelID]

			// Find constraints by matching the capability slug to the hardware pipeline
			capID := slugToCapID[pName]

			modelInfo, hasModelInfo := func() (types.NetworkCapabilityModelInfo, bool) {
				if capID == "" {
					return types.NetworkCapabilityModelInfo{}, false
				}
				cap, ok := orch.Capabilities.Constraints.PerCapability[capID]
				if !ok {
					return types.NetworkCapabilityModelInfo{}, false
				}
				mi, ok := cap.Models[hw.ModelID]
				return mi, ok
			}()

			warm := false
			capacity := 0
			runnerVersion := ""
			if hasModelInfo {
				warm = modelInfo.Warm
				capacity = modelInfo.Capacity
				runnerVersion = strings.TrimSpace(modelInfo.RunnerVersion)
				if runnerVersion != "" {
					orchRunnerVersions = append(orchRunnerVersions, runnerVersion)
					globalRunnerVersions = append(globalRunnerVersions, runnerVersion)
				}
			} else {
				missingConstraintCount++
				totalMissingConstraints++
			}

			// Populate returned structure (dedupe pipeline/model)
			if !alreadyAdded {
				modelCount++
				totalModels++
				globalModelSet[hw.ModelID] = true

				m := types.Model{
					Name:          hw.ModelID,
					Warm:          warm,
					IdleCapacity:  capacity,
					CapacityInUse: modelInfo.CapacityInUse,
					RunnerVersion: runnerVersion,
				}

				// Store capacity in Warm/Cold buckets (still compatible with existing code)
				if warm {
					if capacity > 0 {
						m.Status.Warm = capacity
					} else {
						m.Status.Warm = 1
					}
				} else {
					if capacity > 0 {
						m.Status.Cold = capacity
					} else {
						m.Status.Cold = 1
					}
				}

				p.Models = append(p.Models, m)
				seenModel[pName][hw.ModelID] = true
			}

			// GPU info logging (if available) + count
			if len(hw.GPUInfo) > 0 {
				// make logging stable (nice-to-have)
				keys := make([]string, 0, len(hw.GPUInfo))
				for k := range hw.GPUInfo {
					keys = append(keys, k)
				}
				sort.Strings(keys)

				gpuCount += len(hw.GPUInfo)
				totalGPUs += len(hw.GPUInfo)

				for _, idx := range keys {
					gpu := hw.GPUInfo[idx]
					s.logger.DebugContext(ctx, "orch model capability",
						slog.String("orch", orchAddr),
						slog.String("pipeline", pName),
						slog.String("model", hw.ModelID),
						slog.Bool("warm", warm),
						slog.Int("capacity", capacity),
						slog.String("runnerVersion", runnerVersion),
						slog.String("gpuIndex", idx),
						slog.String("gpuName", gpu.Name),
						slog.Uint64("memoryFree", gpu.MemoryFree),
						slog.Uint64("memoryTotal", gpu.MemoryTotal),
						slog.Bool("hasConstraint", hasModelInfo),
					)
				}
			} else {
				s.logger.DebugContext(ctx, "orch model capability",
					slog.String("orch", orchAddr),
					slog.String("pipeline", pName),
					slog.String("model", hw.ModelID),
					slog.Bool("warm", warm),
					slog.Int("capacity", capacity),
					slog.String("runnerVersion", runnerVersion),
					slog.Bool("hasConstraint", hasModelInfo),
				)
			}
		}

		// Attach pipelines to orchCap (even if empty, we still include the orch)
		for _, p := range pipelineByName {
			orchCap.Pipelines = append(orchCap.Pipelines, *p)
		}
		pipelines.Orchestrators = append(pipelines.Orchestrators, orchCap)

		// Update global pipeline count by unique pipelines observed per orch
		totalPipelines += pipelineCount

		// Per-orchestrator runnerVersion min/max (semver-ish)
		minRV, maxRV := minMaxSemverish(orchRunnerVersions)

		// Per-orchestrator summary line (WARN if missing constraints)
		if missingConstraintCount > 0 {
			orchsWithMissingConstraints++
			s.logger.WarnContext(ctx, "pipelines discovered for orch (constraints missing)",
				slog.String("orch", orchAddr),
				slog.Int("pipelines", pipelineCount),
				slog.Int("models", modelCount),
				slog.Int("gpus", gpuCount),
				slog.Int("missingConstraints", missingConstraintCount),
				slog.String("runnerVersionMin", minRV),
				slog.String("runnerVersionMax", maxRV),
			)
		} else {
			s.logger.InfoContext(ctx, "pipelines discovered for orch",
				slog.String("orch", orchAddr),
				slog.Int("pipelines", pipelineCount),
				slog.Int("models", modelCount),
				slog.Int("gpus", gpuCount),
				slog.Int("missingConstraints", missingConstraintCount),
				slog.String("runnerVersionMin", minRV),
				slog.String("runnerVersionMax", maxRV),
			)
		}
	}

	// -------- Global summary (nice-to-have) --------
	globalMinRV, globalMaxRV := minMaxSemverish(globalRunnerVersions)

	s.logger.InfoContext(ctx, "pipelines discovery complete",
		slog.Int("orchs", totalOrchs),
		slog.Int("orchsWithMissingConstraints", orchsWithMissingConstraints),
		slog.Int("totalMissingConstraints", totalMissingConstraints),
		slog.Int("totalPipelinesObserved", totalPipelines),
		slog.Int("totalModelsObserved", totalModels),
		slog.Int("totalGPUsObserved", totalGPUs),
		slog.Int("distinctPipelines", len(globalPipelineSet)),
		slog.Int("distinctModels", len(globalModelSet)),
		slog.String("runnerVersionMin", globalMinRV),
		slog.String("runnerVersionMax", globalMaxRV),
	)

	return &pipelines, nil
}

// slugifyCapabilityName normalizes a capability name from `capabilities_names` into
// the `hardware.pipeline` style used by the broadcaster CLI.
func slugifyCapabilityName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.Join(strings.Fields(s), "-")
	return s
}

// minMaxSemverish returns min/max runner versions from a list.
// - Tries to compare as semver-ish numeric dotted versions (e.g., "0.14.1").
// - Falls back to lexical compare if parsing fails.
func minMaxSemverish(versions []string) (string, string) {
	clean := make([]string, 0, len(versions))
	for _, v := range versions {
		v = strings.TrimSpace(v)
		if v != "" {
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 {
		return "", ""
	}

	minV := clean[0]
	maxV := clean[0]
	for _, v := range clean[1:] {
		if semverishLess(v, minV) {
			minV = v
		}
		if semverishLess(maxV, v) {
			maxV = v
		}
	}
	return minV, maxV
}

func semverishLess(a, b string) bool {
	pa, oka := parseSemverish(a)
	pb, okb := parseSemverish(b)
	if oka && okb {
		n := len(pa)
		if len(pb) > n {
			n = len(pb)
		}
		for i := 0; i < n; i++ {
			ai := 0
			bi := 0
			if i < len(pa) {
				ai = pa[i]
			}
			if i < len(pb) {
				bi = pb[i]
			}
			if ai != bi {
				return ai < bi
			}
		}
		// identical numerically; shorter string considered "smaller"
		return len(a) < len(b)
	}
	// fallback lexical
	return a < b
}

func parseSemverish(v string) ([]int, bool) {
	// strip leading 'v'
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "v") || strings.HasPrefix(v, "V") {
		v = v[1:]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		// stop at first non-numeric tail (e.g. "0.14.1-rc1")
		numStr := p
		for i := 0; i < len(numStr); i++ {
			if numStr[i] < '0' || numStr[i] > '9' {
				numStr = numStr[:i]
				break
			}
		}
		if numStr == "" {
			return nil, false
		}
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
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
