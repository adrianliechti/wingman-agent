package harness

import "context"

type inputIDKey struct{}

// WithInputID correlates accepted user input with its retained conversation
// message. This is local metadata, never part of a model provider's payload.
func WithInputID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, inputIDKey{}, id)
}
func InputIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(inputIDKey{}).(string)
	return id
}

type inputContextKey struct{}

// WithInputContext gives an input text that the model reads before it and no
// transcript shows, such as an attached chat. The harness records it with the
// input, so it never outlives an input that was refused.
func WithInputContext(ctx context.Context, texts []string) context.Context {
	return context.WithValue(ctx, inputContextKey{}, texts)
}

func InputContextFromContext(ctx context.Context) []string {
	texts, _ := ctx.Value(inputContextKey{}).([]string)
	return texts
}
