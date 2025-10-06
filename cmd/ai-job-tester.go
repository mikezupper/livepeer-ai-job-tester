package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/ffmpeg"
	"livepeer-job-tester/internal/gateway/status"
	"livepeer-job-tester/internal/logging"
	"livepeer-job-tester/internal/server"
	"livepeer-job-tester/internal/services"
)

// main is the entry point of the application. It loads the configuration file, sets up the HTTP client,
// initializes the Livepeer service, and starts the embedded webhook server. It also invokes the test job logic.
func main() {
	// Parse command-line flags to get the configuration file path.
	configFile := flag.String("f", "configs/config.json", "path to the config file")
	flag.Parse()

	ctx := context.Background()

	// Load the configuration file.
	configLoader := &config.JSONConfigLoader{}
	cfg, err := configLoader.Load(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	loggerCfg := logging.Config{Level: "info"}
	if cfg.Logger != nil {
		loggerCfg.Level = cfg.Logger.Level
		loggerCfg.Format = cfg.Logger.Format
		loggerCfg.Modules = cfg.Logger.Modules
	}

	loggerManager, err := logging.NewManager(loggerCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating logger: %v\n", err)
		os.Exit(1)
	}

	appLogger := loggerManager.Logger("app")

	// Create an HTTP client with a custom transport.
	client := createHTTPClient()

	// Initialize the Livepeer service with the HTTP client and loaded configuration.
	livepeerService := services.NewHTTPLivepeerService(client, cfg, loggerManager.Logger("livepeer"))
	statusClient := status.NewClient(nil, loggerManager.Logger("gateway-status"))
	ffmpegClient, err := ffmpeg.NewClient(loggerManager.Logger("ffmpeg"), statusClient)
	if err != nil {
		appLogger.Error("failed to construct ffmpeg client", slog.Any("error", err))
		os.Exit(1)
	}
	webhookServer, err := server.NewEmbeddedWebhookServer(cfg, client, livepeerService, ffmpegClient, loggerManager.Logger("server"))
	if err != nil {
		appLogger.Error("failed to construct webhook server", slog.Any("error", err))
		os.Exit(1)
	}

	// Build the address for the server based on the configuration.
	addr := fmt.Sprintf("%s:%s", cfg.InternalWebServerAddress, cfg.InternalWebServerPort)

	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	// Start the server in a separate goroutine to handle requests.
	go func() {
		if err := webhookServer.StartServer(serverCtx, addr); err != nil {
			appLogger.ErrorContext(serverCtx, "server exited with error", slog.Any("error", err))
			serverCancel()
		}
	}()

	// Run the logic to fetch orchestrators, pipelines, and send test jobs.
	if err := webhookServer.RunTestJobs(ctx); err != nil {
		appLogger.ErrorContext(ctx, "error running test jobs", slog.Any("error", err))
		os.Exit(1)
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
