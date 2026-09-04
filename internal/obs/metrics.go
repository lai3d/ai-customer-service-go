// Package obs holds the metrics and traces. Token spend and latency are the two numbers
// that decide whether an LLM feature survives contact with production.
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics are tagged by model and never by conversation id.
//
// Per-conversation tags grow cardinality without limit and take the metrics backend
// down long before the bill does. There is deliberately no way to pass a conversation
// id into any of these.
type Metrics struct {
	Registry *prometheus.Registry

	Tokens      *prometheus.CounterVec
	CostUSD     *prometheus.CounterVec
	ModelCalls  *prometheus.CounterVec
	Turns       *prometheus.CounterVec
	TurnSeconds *prometheus.HistogramVec
	ToolCalls   *prometheus.CounterVec
	Unpriced    *prometheus.CounterVec
	Retrieval   prometheus.Histogram
	Embedding   prometheus.Histogram
}

func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		Registry: registry,
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chat_tokens_total",
			Help: "Tokens billed, by model and direction.",
		}, []string{"model", "type"}),
		CostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chat_cost_usd_total",
			Help: "Estimated spend in USD, by model. Stays at zero for a model with no price entry.",
		}, []string{"model"}),
		ModelCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chat_model_calls_total",
			Help: "Model calls. A tool-calling turn makes at least two, and each is billed.",
		}, []string{"model", "outcome"}),
		Turns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chat_turns_total",
			Help: "Customer turns, by how they ended.",
		}, []string{"outcome"}),
		TurnSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "chat_turn_duration_seconds",
			Help:    "Wall time of a whole customer turn, retrieval and every model call included.",
			Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 16, 32, 64},
		}, []string{"model"}),
		ToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chat_tool_invocations_total",
			Help: "Tool invocations, by outcome.",
		}, []string{"tool", "outcome"}),
		// Without this, a model with no price entry is indistinguishable from a model
		// that cost nothing: tokens keep counting and chat_cost_usd_total stays at
		// zero. That is the failure mode of keying prices on the requested model id
		// when the provider reports a dated one -- asking for gpt-5 yields
		// gpt-5-2025-08-07 -- and it is silent unless something counts it.
		Unpriced: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chat_unpriced_model_calls_total",
			Help: "Model calls whose tokens were counted but could not be costed, by model.",
		}, []string{"model"}),
		Retrieval: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "chat_retrieval_duration_seconds",
			Help:    "Query embedding plus vector search.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1},
		}),
		Embedding: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "chat_embedding_duration_seconds",
			Help:    "One in-process embedding forward pass.",
			Buckets: []float64{0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1},
		}),
	}
	registry.MustRegister(m.Tokens, m.CostUSD, m.ModelCalls, m.Turns,
		m.TurnSeconds, m.ToolCalls, m.Unpriced, m.Retrieval, m.Embedding)
	return m
}

// RecordUsage meters one model call. The model is the one the provider reported.
func (m *Metrics) RecordUsage(model string, inputTokens, outputTokens int64, usd float64, priced bool) {
	m.Tokens.WithLabelValues(model, "input").Add(float64(inputTokens))
	m.Tokens.WithLabelValues(model, "output").Add(float64(outputTokens))
	if priced {
		m.CostUSD.WithLabelValues(model).Add(usd)
		return
	}
	m.Unpriced.WithLabelValues(model).Inc()
}
