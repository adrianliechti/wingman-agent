package opencode

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/external"
)

type Options = external.Options

func NewConfig(ctx context.Context, options *Options) (string, error) {
	options = external.WithDefaults(options)

	available, err := external.AvailableModels(ctx, options)

	if err != nil {
		return "", err
	}

	var mainModel string
	var smallModel string

	models := make(map[string]any)

	isSmall := func(name string) bool {
		lower := strings.ToLower(name)

		for _, kw := range []string{"mini", "flash", "small", "haiku", "spark"} {
			if strings.Contains(lower, kw) {
				return true
			}
		}

		return false
	}

	for _, g := range candidates {
		for _, m := range g.models {
			if !available[m.id] {
				continue
			}

			models[m.id] = map[string]any{
				"name": g.name,

				"limit": map[string]any{
					"context": m.inputTokens,
					"output":  m.outputTokens,
				},
			}

			if isSmall(g.name) {
				if smallModel == "" {
					smallModel = m.id
				}
			} else {
				if mainModel == "" {
					mainModel = m.id
				}
			}

			break
		}
	}

	if mainModel == "" {
		mainModel = smallModel
	}

	if smallModel == "" {
		smallModel = mainModel
	}

	url := strings.TrimRight(options.WingmanURL, "/") + "/v1"

	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",

		"model":       "wingman/" + mainModel,
		"small_model": "wingman/" + smallModel,

		"enabled_providers": []string{"wingman"},

		"autoupdate": false,
		"share":      "disabled",
		"snapshot":   false,

		"provider": map[string]any{
			"wingman": map[string]any{
				"npm": "@ai-sdk/openai-compatible",

				"name": "Wingman",

				"options": map[string]any{
					"baseURL": url,
					"apiKey":  options.WingmanToken,
				},

				"models": models,
			},
		},
	}

	data, _ := json.Marshal(cfg)

	return string(data), nil
}
