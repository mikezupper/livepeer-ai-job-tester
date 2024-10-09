package types

// Stats represents the raw statistics per test stream, capturing details such as
// the region, pipeline used, model details, success rate, and round-trip time.
// It also stores errors encountered during the test and a timestamp.
type Stats struct {
	Region          string  `json:"region"`
	Pipeline        string  `json:"pipeline"`
	Model           string  `json:"model"`
	ModelIsWarm     bool    `json:"model_is_warm"`
	InputParameters string  `json:"input_parameters"`
	ResponsePayload string  `json:"response_payload"`
	Orchestrator    string  `json:"orchestrator"`
	SuccessRate     int     `json:"success_rate"`
	RoundTripTime   float64 `json:"round_trip_time"`
	Errors          []Error `json:"errors"`
	Timestamp       int64   `json:"timestamp"`
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

// WarmStatus represents the warm status of a pipeline, indicating if the pipeline
// is preloaded and ready for faster execution.
type WarmStatus struct {
	Warm bool `json:"Warm"`
}

// Pipeline represents the structure of pipelines associated with an orchestrator.
// It uses a map to represent pipeline names, with each containing another map of
// models and their corresponding warm status.
type Pipeline map[string]map[string]WarmStatus

// Orchestrators represents a collection of orchestrators, where each orchestrator
// contains a set of pipelines.
type Orchestrators map[string]struct {
	Pipelines Pipeline `json:"Pipelines"`
}

// SupportedPipelineStatus represents the status of a pipeline, with separate counts
// for cold and warm instances.
type SupportedPipelineStatus struct {
	Cold int `json:"Cold"`
	Warm int `json:"Warm"`
}

// SupportedPipelines represents the supported pipelines and their corresponding statuses,
// organized by orchestrator and pipeline.
type SupportedPipelines map[string]map[string]SupportedPipelineStatus

// Pipelines represents the top-level structure that contains both orchestrators
// and the supported pipelines. This structure combines orchestrator data and
// pipeline statuses.
type Pipelines struct {
	Orchestrators      Orchestrators      `json:"orchestrators"`
	SupportedPipelines SupportedPipelines `json:"supported_pipelines"`
}
