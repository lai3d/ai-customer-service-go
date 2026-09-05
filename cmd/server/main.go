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

	"github.com/lai3d/ai-customer-service-go/internal/admin"
	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/store"
	"github.com/lai3d/ai-customer-service-go/internal/ticket"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	// A container healthcheck without adding curl to the image. The alternative is a
	// shell and an HTTP client in the runtime layer for one GET.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

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

// healthcheck asks the local server whether it is ready. Exit code 0 means yes.
func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + addr + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "readyz returned %d\n", resp.StatusCode)
		return 1
	}
	return 0
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

	shutdownTracing, err := obs.StartTracing(ctx, obs.TracingOptions{
		Enabled:     cfg.Obs.OTLPEnabled,
		Endpoint:    cfg.Obs.OTLPEndpoint,
		ServiceName: "ai-customer-service-go",
		Sampling:    cfg.Obs.TraceSampling,
	})
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			slog.Warn("could not flush traces on shutdown", "error", err)
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelStartup()

	pool, err := store.Open(startupCtx, cfg.Postgres.URL(), cfg.Postgres.MaxConns, cfg.RAG.Dimensions)
	if err != nil {
		return err
	}
	defer pool.Close()

	onnx, err := rag.NewONNXEmbedder(rag.ONNXOptions{
		ModelPath:     cfg.RAG.ModelPath,
		TokenizerPath: cfg.RAG.TokenizerPath,
		Dimensions:    cfg.RAG.Dimensions,
		QueryPrefix:   cfg.RAG.QueryPrefix,
		PassagePrefix: cfg.RAG.PassagePrefix,
	})
	if err != nil {
		return err
	}
	defer onnx.Close()

	// Bounded on measurement, not on principle: see internal/rag/bounded.go.
	embedder := rag.NewBounded(onnx, cfg.RAG.MaxConcurrentEmbeddings)

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

	tickets := ticket.NewStore(pool)

	service := chat.NewService(
		chat.NewMemory(pool, cfg.Chat.MaxHistoryMessages),
		rag.NewRetriever(embedder, vectors, cfg.RAG.TopK, cfg.RAG.SimilarityThreshold),
		client,
		cost.NewBudget(cfg.Cost.ConversationTokenBudget, cfg.Cost.TrackedConversations),
		metrics,
		chat.NewRecorder(pool),
		cfg.Chat.MaxTokens,
		tools.NewOrderLookup(),
		tools.NewSupportTickets(tickets),
	)

	mux := http.NewServeMux()
	httpapi.NewServer(service, cfg.Chat).Routes(mux)

	// The operations surface, or nothing at all.
	//
	// With no operator configured the routes are never registered, so /admin and
	// /api/admin/v1/* are 404 in the ordinary way rather than 401 from a guard. A guard
	// is a thing that can be misconfigured; an absent route cannot be.
	operators, err := admin.ParseOperators(cfg.Admin.Tokens)
	if err != nil {
		return fmt.Errorf("ADMIN_TOKENS: %w", err)
	}
	if operators.Enabled() {
		admin.NewServer(admin.NewStore(pool), tickets, operators,
			admin.ParseCORS(cfg.Admin.CORSOrigins)).Routes(mux)
		slog.Info("operations API mounted at /api/admin/v1; the UI is admin-ui/, served separately",
			"operators", operators.Names(), "cors_origins", cfg.Admin.CORSOrigins)
	} else {
		slog.Info("no operations surface: ADMIN_TOKENS is unset")
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.Handle("GET /", httpapi.DemoUI())
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
		Addr: cfg.HTTPAddr,
		// otelhttp gives every request a server span, so a turn's spans hang off the
		// HTTP request that caused them rather than floating on their own.
		Handler: otelhttp.NewHandler(mux, "http",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			})),
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
