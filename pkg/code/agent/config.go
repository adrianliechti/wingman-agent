package agent

import "github.com/adrianliechti/wingman-agent/pkg/harness"

// Config holds the startup settings for New.
type Config struct {
	harness.Config
	Options
}

// DefaultConfig loads settings from the environment; the caller must shut down Telemetry after closing its agents.
func DefaultConfig() (Config, error) {
	cfg, err := harness.ConfigFromEnvironment()
	if err != nil {
		return Config{}, err
	}
	return Config{Config: *cfg, Options: optionsFromEnvironment()}, nil
}
