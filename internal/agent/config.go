package agent

import (
	"fmt"
	"os"
)

type Config struct {
	BundleURL    string
	BundleDigest string
}

func LoadConfig() (Config, error) {
	cfg := Config{
		BundleURL:    os.Getenv("KIT_AGENT_BUNDLE_URL"),
		BundleDigest: os.Getenv("KIT_AGENT_BUNDLE_SHA256"),
	}
	if cfg.BundleURL == "" {
		return Config{}, fmt.Errorf("KIT_AGENT_BUNDLE_URL is required")
	}
	if cfg.BundleDigest == "" {
		return Config{}, fmt.Errorf("KIT_AGENT_BUNDLE_SHA256 is required")
	}
	return cfg, nil
}
