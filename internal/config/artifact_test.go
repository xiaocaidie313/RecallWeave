package config

import "testing"

func TestArtifactDirPrecedence(t *testing.T) {
	t.Setenv("RECALLWEAVE_ARTIFACTS_DIR", "")
	var cfg *Config
	if cfg.ArtifactDir() != "data/artifacts" {
		t.Fatal("nil config must use default directory")
	}
	cfg = &Config{Artifacts: ArtifactsConfig{Dir: "custom/outputs"}}
	if cfg.ArtifactDir() != "custom/outputs" {
		t.Fatal("configured directory ignored")
	}
	t.Setenv("RECALLWEAVE_ARTIFACTS_DIR", "env/outputs")
	if cfg.ArtifactDir() != "env/outputs" {
		t.Fatal("environment must override config")
	}
}
