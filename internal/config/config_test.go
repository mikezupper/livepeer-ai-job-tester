package config

import "testing"

func TestNormalizeAndValidateRejectsMissingPromptVariantsForLiveVideo(t *testing.T) {
	cfg := &Config{
		Pipelines: []Pipeline{
			{
				Name: "Live video to video",
				Uri:  "live-video-to-video",
				Live: true,
			},
		},
	}

	if err := normalizeAndValidate(cfg); err == nil {
		t.Fatalf("expected validation error for missing promptVariants")
	}
}

func TestNormalizeAndValidateRejectsInvalidPromptComplexity(t *testing.T) {
	cfg := &Config{
		Pipelines: []Pipeline{
			{
				Name: "Live video to video",
				Uri:  "live-video-to-video",
				Live: true,
				PromptVariants: []PromptVariant{
					{ID: "prompt-a", Complexity: "extreme"},
				},
			},
		},
	}

	if err := normalizeAndValidate(cfg); err == nil {
		t.Fatalf("expected validation error for invalid complexity")
	}
}

func TestNormalizeAndValidateAppliesDebugArtifactDefaults(t *testing.T) {
	cfg := &Config{
		LiveVideo: &LiveVideoConfig{
			DebugArtifacts: &DebugArtifactsConfig{Enabled: true},
		},
		Pipelines: []Pipeline{
			{
				Name: "Live video to video",
				Uri:  "live-video-to-video",
				Live: true,
				PromptVariants: []PromptVariant{
					{ID: "prompt-a", Complexity: "low"},
				},
			},
		},
	}

	if err := normalizeAndValidate(cfg); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if got, want := cfg.LiveVideo.DebugArtifacts.MaxFrames, 3; got != want {
		t.Fatalf("maxFrames default = %d, want %d", got, want)
	}
	if got, want := cfg.LiveVideo.DebugArtifacts.OutputDir, "debug-artifacts/live-video"; got != want {
		t.Fatalf("outputDir default = %q, want %q", got, want)
	}
}
