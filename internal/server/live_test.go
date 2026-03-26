package server

import (
	"context"
	"log/slog"
	"net/url"
	"testing"

	"livepeer-job-tester/internal/config"
	"livepeer-job-tester/internal/services"
	"livepeer-job-tester/internal/types"
)

func TestBuildLiveIngestURLEncodesSingleParamsField(t *testing.T) {
	ingestURL, err := buildLiveIngestURL(
		"rtmp://localhost:1935",
		"aiJobTesterStream-low-prompt-123",
		"streamdiffusion-model",
		"https://orch.example:8935",
		`{"prompt":"rainy city","width":512}`,
	)
	if err != nil {
		t.Fatalf("buildLiveIngestURL() error = %v", err)
	}

	parsed, err := url.Parse(ingestURL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	q := parsed.Query()
	if got, want := q.Get("pipeline"), "streamdiffusion-model"; got != want {
		t.Fatalf("pipeline query = %q, want %q", got, want)
	}
	if got := q.Get("params"); got != `{"prompt":"rainy city","width":512}` {
		t.Fatalf("params query = %q, want raw params JSON", got)
	}
	if got := q.Get("prompt"); got != "" {
		t.Fatalf("unexpected flattened prompt query param %q", got)
	}
}

func TestBuildExecutionPlanExpandsPromptVariantsForLiveVideo(t *testing.T) {
	ss := &EmbeddedWebhookServer{
		config: &config.Config{
			Pipelines: []config.Pipeline{
				{
					Name: "Live video to video",
					Uri:  "live-video-to-video",
					Live: true,
					PromptVariants: []config.PromptVariant{
						{ID: "low-prompt", Complexity: "low"},
						{ID: "high-prompt", Complexity: "high"},
					},
				},
			},
		},
		jobTesterMetrics: services.NewJobTesterMetrics(),
		logger:           slog.Default(),
	}

	orchestrators := []types.Orchestrator{
		{Address: "0xorch", ServiceURI: "https://orch.example:8935", Active: true},
	}
	capabilityMap := map[string]types.OrchestratorCapability{
		"0xorch": {
			Address: "0xorch",
			Pipelines: []types.Pipeline{
				{
					Type: "live-video-to-video",
					Models: []types.Model{
						{Name: "streamdiffusion-model", Status: types.Status{Warm: 1}},
					},
				},
			},
		},
	}

	standardJobs, liveBundles := ss.buildExecutionPlan(context.Background(), orchestrators, capabilityMap)
	if len(standardJobs) != 0 {
		t.Fatalf("standard job count = %d, want 0", len(standardJobs))
	}
	if len(liveBundles) != 1 {
		t.Fatalf("live bundle count = %d, want 1", len(liveBundles))
	}
	if got, want := len(liveBundles[0].Prompts), 2; got != want {
		t.Fatalf("prompt count = %d, want %d", got, want)
	}
	if got, want := ss.jobTesterMetrics.ExpectedTotalJobs, 2; got != want {
		t.Fatalf("expected job count = %d, want %d", got, want)
	}
}
