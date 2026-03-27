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

func TestNormalizeAndValidateInitializesMissingPipelineParameters(t *testing.T) {
	cfg := &Config{
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
	if cfg.Pipelines[0].Parameters == nil {
		t.Fatal("expected pipeline parameters to be initialized")
	}
}
