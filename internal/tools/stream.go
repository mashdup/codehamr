package tools

import "context"

// OutputSink receives raw output chunks from a streaming tool (bash today) as
// they arrive, before the command finishes. The bytes are only valid for the
// duration of the call — the caller reuses the backing buffer — so a sink that
// retains them must copy. Optional: nil when no consumer is registered, in
// which case the tool runs fully buffered as before.
type OutputSink func([]byte)

type sinkKey struct{}

// WithOutputSink attaches a live-output sink to ctx. A streaming tool tees each
// write to it in addition to its buffered result, letting the protocol driver
// forward incremental output to the UI.
func WithOutputSink(ctx context.Context, sink OutputSink) context.Context {
	return context.WithValue(ctx, sinkKey{}, sink)
}

// outputSink returns the sink attached to ctx, or nil.
func outputSink(ctx context.Context) OutputSink {
	if s, ok := ctx.Value(sinkKey{}).(OutputSink); ok {
		return s
	}
	return nil
}
