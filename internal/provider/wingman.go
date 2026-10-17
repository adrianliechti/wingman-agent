package provider

import (
	"os"
	"strings"
)

const DefaultWingmanURL = "http://localhost:4242"

// Wingman resolves explicit overrides, environment values, then local defaults.
// The URL is the gateway root; callers append their protocol-specific paths.
func Wingman(baseURL, token string) (string, string) {
	baseURL = firstValue(baseURL, os.Getenv("WINGMAN_URL"), DefaultWingmanURL)
	token = firstValue(token, os.Getenv("WINGMAN_TOKEN"), "-")
	return strings.TrimRight(baseURL, "/"), token
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
