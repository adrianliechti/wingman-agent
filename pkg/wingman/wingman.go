package wingman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

const MaxFetchBytes = 100 * 1024

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func FromEnv() (*Client, error) {
	baseURL := os.Getenv("WINGMAN_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("WINGMAN_URL is not configured")
	}

	return &Client{
		BaseURL: baseURL,
		Token:   os.Getenv("WINGMAN_TOKEN"),
	}, nil
}

func (c *Client) Fetch(ctx context.Context, url, prompt string) (string, bool, error) {
	data, err := c.postForm(ctx, "/v1/extract", []formField{
		{Name: "url", Value: url},
		{Name: "prompt", Value: prompt},
	}, MaxFetchBytes)
	if err != nil {
		return "", false, fmt.Errorf("extract API returned %w", err)
	}

	content := strings.TrimSpace(string(data))
	truncated := len(data) > MaxFetchBytes
	if truncated {
		content = content[:MaxFetchBytes]
	}

	if content == "" {
		return "", truncated, fmt.Errorf("empty response from extract API")
	}

	return content, truncated, nil
}

func (c *Client) Search(ctx context.Context, query string, allowedDomains, blockedDomains []string) ([]SearchResult, string, error) {
	fields := []formField{{Name: "query", Value: query}}

	if len(allowedDomains) > 0 {
		data, err := json.Marshal(allowedDomains)
		if err != nil {
			return nil, "", err
		}
		fields = append(fields, formField{Name: "allowed_domains", Value: string(data)})
	}

	if len(blockedDomains) > 0 {
		data, err := json.Marshal(blockedDomains)
		if err != nil {
			return nil, "", err
		}
		fields = append(fields, formField{Name: "blocked_domains", Value: string(data)})
	}

	data, err := c.postForm(ctx, "/v1/search", fields, 0)
	if err != nil {
		return nil, "", fmt.Errorf("search API returned %w", err)
	}

	var structured struct {
		Results []SearchResult `json:"results"`
	}

	if err := json.Unmarshal(data, &structured); err == nil && len(structured.Results) > 0 {
		return structured.Results, "", nil
	}

	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, "", fmt.Errorf("empty response from search API")
	}

	return nil, text, nil
}

type formField struct {
	Name  string
	Value string
}

func (c *Client) postForm(ctx context.Context, path string, fields []formField, maxBytes int) ([]byte, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + path

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for _, field := range fields {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
			return nil, err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	reader := io.Reader(resp.Body)
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, int64(maxBytes)+1)
	}

	return io.ReadAll(reader)
}
