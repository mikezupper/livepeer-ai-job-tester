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

// Status represents the status of a model with counts for cold and warm instances.
type Status struct {
	Cold int `json:"Cold"`
	Warm int `json:"Warm"`
}

// Model represents a model within a pipeline, including its name and status.
type Model struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
}

// Pipeline represents a pipeline, including its type and the models it contains.
type Pipeline struct {
	Type   string  `json:"type"`
	Models []Model `json:"models"`
}

// Orchestrator represents an orchestrator, including its address and pipelines.
type OrchestratorCapability struct {
	Address   string     `json:"address"`
	Pipelines []Pipeline `json:"pipelines"`
}

// Pipelines is the top-level structure that contains all orchestrators.
type Pipelines struct {
	Orchestrators []OrchestratorCapability `json:"orchestrators"`
}
