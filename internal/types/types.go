package types

import "encoding/json"

// Stats represents the raw statistics per test stream, capturing details such as
// the region, pipeline used, model details, success rate, and round-trip time.
// It also stores errors encountered during the test and a timestamp.
type Stats struct {
	Region   string `json:"region"`
	Pipeline string `json:"pipeline"`
	// Model stores the capability-advertised model name. For live runs this is
	// the top-level gateway/worker selector, not an inner runner model_id override.
	Model              string  `json:"model"`
	ModelIsWarm        bool    `json:"model_is_warm"`
	InputParameters    string  `json:"input_parameters"`
	ResponsePayload    string  `json:"response_payload"`
	Orchestrator       string  `json:"orchestrator"`
	SuccessRate        int     `json:"success_rate"`
	RoundTripTime      float64 `json:"round_trip_time"`
	Errors             []Error `json:"errors"`
	Timestamp          int64   `json:"timestamp"`
	TestOutcome        string  `json:"test_outcome,omitempty"`
	UnscoredReason     string  `json:"unscored_reason,omitempty"`
	PromptID           string  `json:"prompt_id,omitempty"`
	PromptComplexity   string  `json:"prompt_complexity,omitempty"`
	PromptVerification string  `json:"prompt_verification,omitempty"`
	PromptConfirmed    *bool   `json:"prompt_confirmed,omitempty"`
	StreamValid        *bool   `json:"stream_valid,omitempty"`
	StreamID           string  `json:"stream_id,omitempty"`
	ParamsHash         string  `json:"params_hash,omitempty"`
	DeferAttempts      int     `json:"defer_attempts,omitempty"`

	OmitScore bool `json:"-"`
}

// MarshalJSON preserves the historical stats shape while allowing live runs to
// omit score fields for outcomes that should not affect leaderboard scoring.
func (s Stats) MarshalJSON() ([]byte, error) {
	type alias Stats
	payload := map[string]interface{}{}

	raw, err := json.Marshal(alias(s))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if s.OmitScore {
		delete(payload, "success_rate")
		delete(payload, "round_trip_time")
	}
	return json.Marshal(payload)
}

// Error represents the details of an error encountered during a test job.
// It includes an error code, a message describing the error, and the count of occurrences.
type Error struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	Count     int    `json:"count"`
}

// Orchestrator represents the data related to a single orchestrator.
// This includes its address, service URI, reward details, stake, and status.
type Orchestrator struct {
	Address                    string  `json:"Address"`
	ServiceURI                 string  `json:"ServiceURI"`
	LastRewardRound            int     `json:"LastRewardRound"`
	RewardCut                  int     `json:"RewardCut"`
	FeeShare                   int     `json:"FeeShare"`
	DelegatedStake             float64 `json:"DelegatedStake"`
	ActivationRound            int     `json:"ActivationRound"`
	DeactivationRound          float64 `json:"DeactivationRound"`
	LastActiveStakeUpdateRound int     `json:"LastActiveStakeUpdateRound"`
	Active                     bool    `json:"Active"`
	Status                     string  `json:"Status"`
	PricePerPixel              string  `json:"PricePerPixel"`
}

// Status represents the status of a model with counts for cold and warm instances.
type Status struct {
	Cold int `json:"Cold"`
	Warm int `json:"Warm"`
}

// Model represents a model within a pipeline, including its name and status.
type Model struct {
	Name          string `json:"name"`
	Status        Status `json:"status"`
	Warm          bool   `json:"warm,omitempty"`
	IdleCapacity  int    `json:"idle_capacity,omitempty"`
	CapacityInUse int    `json:"capacity_in_use,omitempty"`
	RunnerVersion string `json:"runner_version,omitempty"`
}

// Pipeline represents a pipeline, including its type and the models it contains.
type Pipeline struct {
	Type   string  `json:"type"`
	Models []Model `json:"models"`
}

// OrchestratorCapability represents an orchestrator, including its address, service URI, and pipelines.
type OrchestratorCapability struct {
	Address    string     `json:"address"`
	ServiceURI string     `json:"serviceURI"`
	Pipelines  []Pipeline `json:"pipelines"`
}

// Pipelines is the top-level structure that contains all orchestrators.
type Pipelines struct {
	Orchestrators []OrchestratorCapability `json:"orchestrators"`
}
type NetworkCapabilitiesResponse struct {
	CapabilityNames map[string]string     `json:"capabilities_names"`
	Orchestrators   []NetworkOrchestrator `json:"orchestrators"`
}

type NetworkOrchestrator struct {
	Address      string                  `json:"address"`
	LocalAddress string                  `json:"local_address"`
	OrchURI      string                  `json:"orch_uri"`
	Capabilities NetworkOrchCapabilities `json:"capabilities"`
	Hardware     []NetworkHardware       `json:"hardware"`
}

type NetworkOrchCapabilities struct {
	Constraints NetworkOrchConstraints `json:"constraints"`
}

type NetworkOrchConstraints struct {
	MinVersion    string                                    `json:"minVersion"`
	PerCapability map[string]NetworkCapabilityPerCapability `json:"PerCapability"`
}

type NetworkCapabilityPerCapability struct {
	Models map[string]NetworkCapabilityModelInfo `json:"models"`
}

type NetworkCapabilityModelInfo struct {
	Warm          bool   `json:"warm"`
	Capacity      int    `json:"capacity"`
	CapacityInUse int    `json:"capacity_in_use"`
	RunnerVersion string `json:"runnerVersion"`
}

type NetworkHardware struct {
	Pipeline string                 `json:"pipeline"`
	ModelID  string                 `json:"model_id"`
	GPUInfo  map[string]GPUInfoItem `json:"gpu_info"`
}

type GPUInfoItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Major       int    `json:"major"`
	Minor       int    `json:"minor"`
	MemoryFree  uint64 `json:"memory_free"`
	MemoryTotal uint64 `json:"memory_total"`
}
