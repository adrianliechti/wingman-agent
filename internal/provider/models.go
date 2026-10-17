package provider

import (
	"context"
	"time"

	"github.com/openai/openai-go/v3"
)

const modelDiscoveryTimeout = 10 * time.Second

// ModelIDs bounds the entire discovery request, including pagination, response
// bodies and the client's transient retries. Generation keeps its own timeout.
func ModelIDs(ctx context.Context, client openai.Client) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelDiscoveryTimeout)
	defer cancel()

	iter := client.Models.ListAutoPaging(ctx)
	var ids []string
	for iter.Next() {
		ids = append(ids, iter.Current().ID)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
