package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
)

// Config represents the configuration data loaded from the JSON file.
// It includes settings for the region, job type, internal server,
// metrics API, broadcaster endpoints, and a list of pipelines.
type Config struct {
	Region                   string     `json:"region"`
	JobType                  string     `json:"jobType"`
	InternalWebServerPort    string     `json:"internalWebServerPort"`
	InternalWebServerAddress string     `json:"internalWebServerAddress"`
	MetricsApiEndpoint       string     `json:"metricsApiEndpoint"`
	MetricsSecret            string     `json:"metricsSecret"`
	BroadcasterJobEndpoint   string     `json:"broadcasterJobEndpoint"`
	BroadcasterCliEndpoint   string     `json:"broadcasterCliEndpoint"`
	BroadcasterRequestToken  string     `json:"broadcasterRequestToken"`
	Pipelines                []Pipeline `json:"pipelines"`
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
	byteValue, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("[JSONConfigLoader::LoadConfig] error reading JSON file: %w", err)
	}

	// Unmarshal the JSON data into the Config struct
	var config Config
	err = json.Unmarshal(byteValue, &config)
	if err != nil {
		return nil, fmt.Errorf("[JSONConfigLoader::LoadConfig] error unmarshalling JSON: %w", err)
	}

	return &config, nil
}
