package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Config represents the configuration data loaded from the JSON file.
// It includes settings for the region, job type, internal server,
// metrics API, broadcaster endpoints, and a list of pipelines.
type Config struct {
	Region                   string           `json:"region"`
	JobType                  string           `json:"jobType"`
	InternalWebServerPort    string           `json:"internalWebServerPort"`
	InternalWebServerAddress string           `json:"internalWebServerAddress"`
	MetricsApiEndpoint       string           `json:"metricsApiEndpoint"`
	MetricsSecret            string           `json:"metricsSecret"`
	DisableStatsPosting      bool             `json:"disableStatsPosting,omitempty"`
	BroadcasterJobEndpoint   string           `json:"broadcasterJobEndpoint"`
	BroadcasterCliEndpoint   string           `json:"broadcasterCliEndpoint"`
	BroadcasterRequestToken  string           `json:"broadcasterRequestToken"`
	Pipelines                []Pipeline       `json:"pipelines"`
	LiveVideo                *LiveVideoConfig `json:"liveVideo,omitempty"`
	Logger                   *LoggerConfig    `json:"logger,omitempty"`
}

// Pipeline represents a data processing pipeline configuration.
// It includes the name, URI, whether to capture responses,
// the content type, and additional parameters for the pipeline.
type Pipeline struct {
	Name            string                 `json:"name"`
	Uri             string                 `json:"uri"`
	CaptureResponse bool                   `json:"capture_response"`
	ContentType     string                 `json:"contentType"`
	Parameters      map[string]interface{} `json:"parameters"`
	Live            bool                   `json:"live,omitempty"`
	PromptVariants  []PromptVariant        `json:"promptVariants,omitempty"`
}

// LiveVideoConfig captures configuration specific to live video pipeline tests.
type LiveVideoConfig struct {
	MediaServerURL               string              `json:"mediaServerURL"`
	TestVideoPath                string              `json:"testVideoPath"`
	TestDurationSeconds          int                 `json:"testDurationSeconds"`
	StatusPollTimeoutSeconds     int                 `json:"statusPollTimeoutSeconds"`
	StatusPollIntervalSeconds    int                 `json:"statusPollIntervalSeconds,omitempty"`
	MetricRetryDelayMilliseconds int                 `json:"metricRetryDelayMilliseconds,omitempty"`
	MaxMetricAttempts            int                 `json:"maxMetricAttempts,omitempty"`
	MaxProbeAttempts             int                 `json:"maxProbeAttempts,omitempty"`
	OrchMapping                  map[string][]string `json:"orchMapping,omitempty"`
	TargetFPS                    float64             `json:"targetFPS,omitempty"`
	MaxInitialLatencySeconds     float64             `json:"maxInitialLatencySeconds,omitempty"`
}

// PromptVariant defines a single live video prompt scenario to run against an orchestrator.
type PromptVariant struct {
	ID         string                 `json:"id"`
	Complexity string                 `json:"complexity"`
	Parameters map[string]interface{} `json:"parameters"`
}

// LoggerConfig exposes runtime log configuration knobs.
type LoggerConfig struct {
	Level   string            `json:"level"`
	Format  string            `json:"format,omitempty"`
	Modules map[string]string `json:"modules,omitempty"`
}

// Loader defines the interface for loading a configuration from a file.
// Implementations should handle parsing and returning a Config instance.
type Loader interface {
	// Load reads the configuration from the provided file path.
	// It returns the Config struct or an error if loading fails.
	Load(filePath string) (*Config, error)
}

// JSONConfigLoader is an implementation of Loader that loads
// configuration data from a JSON file.
type JSONConfigLoader struct{}

// Load reads the configuration from the specified JSON file.
// It returns the loaded Config struct or an error if the file
// cannot be opened, read, or parsed correctly.
func (l *JSONConfigLoader) Load(filePath string) (*Config, error) {
	// Open the JSON file
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("[JSONConfigLoader::LoadConfig] error opening JSON file: %w", err)
	}
	defer file.Close()

	// Read the file contents
	byteValue, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("[JSONConfigLoader::LoadConfig] error reading JSON file: %w", err)
	}

	// Unmarshal the JSON data into the Config struct
	var config Config
	err = json.Unmarshal(byteValue, &config)
	if err != nil {
		return nil, fmt.Errorf("[JSONConfigLoader::LoadConfig] error unmarshalling JSON: %w", err)
	}

	if err := normalizeAndValidate(&config); err != nil {
		return nil, fmt.Errorf("[JSONConfigLoader::LoadConfig] invalid configuration: %w", err)
	}

	return &config, nil
}

func normalizeAndValidate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}

	for idx := range cfg.Pipelines {
		pipeline := &cfg.Pipelines[idx]
		if pipeline.Parameters == nil {
			pipeline.Parameters = make(map[string]interface{})
		}

		isLiveVideo := pipeline.Uri == "live-video-to-video"
		if len(pipeline.PromptVariants) > 0 && !isLiveVideo {
			return fmt.Errorf("pipeline %q uses promptVariants but only live-video-to-video supports that layout", pipeline.Uri)
		}
		if isLiveVideo {
			if !pipeline.Live {
				return fmt.Errorf("pipeline %q must set live=true", pipeline.Uri)
			}
			if len(pipeline.PromptVariants) == 0 {
				return fmt.Errorf("pipeline %q must define promptVariants", pipeline.Uri)
			}

			seenIDs := make(map[string]struct{}, len(pipeline.PromptVariants))
			for _, variant := range pipeline.PromptVariants {
				id := strings.TrimSpace(variant.ID)
				if id == "" {
					return fmt.Errorf("pipeline %q has a promptVariant with an empty id", pipeline.Uri)
				}
				if _, exists := seenIDs[id]; exists {
					return fmt.Errorf("pipeline %q has duplicate promptVariant id %q", pipeline.Uri, id)
				}
				seenIDs[id] = struct{}{}

				switch strings.ToLower(strings.TrimSpace(variant.Complexity)) {
				case "low", "medium", "high":
				default:
					return fmt.Errorf("pipeline %q promptVariant %q has invalid complexity %q", pipeline.Uri, id, variant.Complexity)
				}
			}
		}
	}

	return nil
}
