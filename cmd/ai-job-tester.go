package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/server"
	"livepeer-job-tester/internal/services"
	"log"
	"net/http"
	"time"
)

// main is the entry point of the application. It loads the configuration file, sets up the HTTP client,
// initializes the Livepeer service, and starts the embedded webhook server. It also invokes the test job logic.
func main() {
	// Parse command-line flags to get the configuration file path.
	configFile := flag.String("f", "configs/config.json", "path to the config file")
	flag.Parse()

	// Load the configuration file.
	configLoader := &config.JSONConfigLoader{}
	cfg, err := configLoader.Load(*configFile)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Create an HTTP client with a custom transport.
	client := createHTTPClient()

	// Initialize the Livepeer service with the HTTP client and loaded configuration.
	livepeerService := services.NewHTTPLivepeerService(client, cfg)

	// Create and start the embedded webhook server.
	webhookServer := server.NewEmbeddedWebhookServer(cfg, client, livepeerService)

	// Build the address for the server based on the configuration.
	addr := fmt.Sprintf("%s:%s", cfg.InternalWebServerAddress, cfg.InternalWebServerPort)

	// Start the server in a separate goroutine to handle requests.
	go func() {
		if err := webhookServer.StartServer(addr); err != nil {
			log.Fatalf("Error starting server: %v", err)
		}
	}()

	// Run the logic to fetch orchestrators, pipelines, and send test jobs.
	if err := webhookServer.RunTestJobs(); err != nil {
		log.Fatalf("Error running test jobs: %v", err)
	}
}

// createHTTPClient creates and returns a new HTTP client with a custom transport configuration.
// It sets the client to skip certificate verification for TLS and sets a 3-minute timeout for requests.
func createHTTPClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Skip TLS certificate verification.
	}
	return &http.Client{
		Timeout:   3 * time.Minute, // Set request timeout to 3 minutes.
		Transport: tr,
	}
}
