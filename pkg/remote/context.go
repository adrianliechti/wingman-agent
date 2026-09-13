package remote

import "context"

type authenticatedTransportKey struct{}

// AuthenticatedTransport is set by the relay transport, never by HTTP headers.
func AuthenticatedTransport(ctx context.Context) bool {
	value, _ := ctx.Value(authenticatedTransportKey{}).(bool)
	return value
}
