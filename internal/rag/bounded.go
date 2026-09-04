package rag

import (
	"context"
	"runtime"
)

// Bounded limits how many goroutines may be inside the native embedding call at once.
//
// This is here because of a measurement, and it is the Go-specific half of the
// concurrency story. A goroutine blocked in a cgo call blocks the OS thread it is on,
// and the Go scheduler's answer to that is to create another one. Under a thousand
// simultaneous arrivals the runtime went to between 146 and 276 OS threads, varying
// run to run with how many embedding calls happened to overlap. Bounding the
// concurrency to GOMAXPROCS holds it at 40, stably.
//
// What it costs, measured: about 8% throughput (612 to 566 requests/second on the
// benchmark). What it also does, which was not the goal: p50 improves from 1.61s to
// 1.42s while p95 worsens slightly -- the queue that forms lets most requests through
// sooner and makes the tail longer. See docs/benchmark.md.
//
// The work behind the bound is CPU-bound anyway, so admitting more goroutines than
// there are cores buys nothing but threads. Java's virtual threads get this behaviour
// by default: the carrier pool is bounded at the core count and a pinning native call
// queues against it. In Go the default is the other end of the same trade, and the
// bound is something you ask for.
type Bounded struct {
	inner Embedder
	slots chan struct{}
}

// NewBounded wraps an embedder. A limit of 0 or less means GOMAXPROCS.
func NewBounded(inner Embedder, limit int) *Bounded {
	if limit <= 0 {
		limit = runtime.GOMAXPROCS(0)
	}
	return &Bounded{inner: inner, slots: make(chan struct{}, limit)}
}

func (b *Bounded) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		// A customer who has already gone away should not wait for a slot.
		return nil, ctx.Err()
	}
	return b.inner.EmbedQuery(ctx, query)
}

func (b *Bounded) EmbedPassages(ctx context.Context, passages []string) ([][]float32, error) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.inner.EmbedPassages(ctx, passages)
}

func (b *Bounded) Dimensions() int { return b.inner.Dimensions() }
func (b *Bounded) Close() error    { return b.inner.Close() }
