// Command server runs the AI customer service backend.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/store"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// A signal-aware context: SIGTERM starts a graceful shutdown rather than severing
	// in-flight streams. A rolling deploy otherwise cuts answers in half.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Configuration failures -- a missing API key in particular -- have to stop the
	// process here. A service that starts, reports itself healthy, is marked ready by
	// Kubernetes, and then 401s every customer request is the worse failure.
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	metrics := obs.NewMetrics()

	startupCtx, cancelStartup := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelStartup()

	pool, err := store.Open(startupCtx, cfg.Postgres.URL(), cfg.Postgres.MaxConns, cfg.RAG.Dimensions)
	if err != nil {
		return err
	}
	defer pool.Close()

	embedder, err := rag.NewONNXEmbedder(rag.ONNXOptions{
		ModelPath:     cfg.RAG.ModelPath,
		TokenizerPath: cfg.RAG.TokenizerPath,
		Dimensions:    cfg.RAG.Dimensions,
		QueryPrefix:   cfg.RAG.QueryPrefix,
		PassagePrefix: cfg.RAG.PassagePrefix,
	})
	if err != nil {
		return err
	}
	defer embedder.Close()

	vectors := rag.NewStore(pool)
	if cfg.RAG.IngestOnStartup {
		if _, err := rag.Ingest(startupCtx, cfg.RAG.CorpusPath, embedder, vectors); err != nil {
			return err
		}
	}

	client, err := llm.New(cfg.Chat)
	if err != nil {
		return err
	}

	service := chat.NewService(
		chat.NewMemory(pool, cfg.Chat.MaxHistoryMessages),
		rag.NewRetriever(embedder, vectors, cfg.RAG.TopK, cfg.RAG.SimilarityThreshold),
		client,
		cost.NewBudget(cfg.Cost.ConversationTokenBudget, cfg.Cost.TrackedConversations),
		metrics,
		cfg.Chat.MaxTokens,
		tools.NewOrderLookup(),
		tools.NewSupportTickets(cfg.Cost.TrackedConversations),
	)

	mux := http.NewServeMux()
	httpapi.NewServer(service, cfg.Chat).Routes(mux)
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"status":"UP"}`)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"status":"DOWN","detail":"database"}`)
			return
		}
		fmt.Fprintln(w, `{"status":"UP"}`)
	})

	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
		// No WriteTimeout: an SSE response is legitimately open for as long as the
		// model keeps talking, and a write deadline would cut long answers off. The
		// read timeouts still bound how long a client may take to send a request.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("listening",
			"addr", cfg.HTTPAddr, "provider", client.Provider(), "model", client.Model())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.Info("shutting down", "grace", cfg.ShutdownTimeout)
	}

	// Stop accepting, let in-flight turns finish. This has to stay below the pod's
	// terminationGracePeriodSeconds, or the container is killed part-way through the
	// grace period it was given.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
