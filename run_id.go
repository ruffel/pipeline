package pipeline

import "context"

type runIDKey struct{}

// withRunID returns a new context carrying the run identifier.
func withRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runIDKey{}, id)
}

// RunIDFrom returns the run identifier configured via [WithRunID], or an
// empty string when none was set. The executor stamps the identifier into the
// run context, so observers can read it from any event context — including
// terminal events (cancellation stripping preserves values) and events
// delivered asynchronously. Steps can read it from their own context too.
func RunIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)

	return id
}
