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
	"github.com/lai3d/ai-customer-service-go/internal/feedback"
	"github.com/lai3d/ai-customer-service-go/internal/handoff"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
	"github.com/lai3d/ai-customer-service-go/internal/identity"
	"github.com/lai3d/ai-customer-service-go/internal/knowledge"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/retention"
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

// orderSource builds the source lookup_order_status reads from, and says which one it is.
//
// Unset ORDER_SERVICE_URL means the fixture, and that is a warning rather than an info
// line on purpose: it is the sentence that has to survive being skimmed. Every number in
// this repository's tool demonstrations came from those five orders.
func orderSource(cfg config.Orders) (tools.OrderSource, error) {
	if cfg.BaseURL == "" {
		slog.Warn("ORDER_SERVICE_URL is unset: lookup_order_status answers from a " +
			"five-order fixture (ORD-10042 to ORD-10046) and reports every other number " +
			"as not found. This is a demo source, not the order system.")
		return tools.NewMemoryOrders(), nil
	}
	source, err := tools.NewHTTPOrders(tools.HTTPOptions{
		BaseURL:  cfg.BaseURL,
		Token:    cfg.Token,
		Timeout:  cfg.Timeout,
		Attempts: cfg.Attempts,
	})
	if err != nil {
		return nil, fmt.Errorf("ORDER_SERVICE_URL: %w", err)
	}
	// The token is not here, and no line anywhere prints it. `authenticated` is the
	// question anyone reading this log actually has.
	slog.Info("lookup_order_status reads the order service",
		"url", cfg.BaseURL, "timeout", cfg.Timeout, "attempts", cfg.Attempts,
		"authenticated", cfg.Token != "")
	return source, nil
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

	// Adopt the bundled corpus as the first managed version, without re-embedding it.
	//
	// Idempotent, and it runs every start-up: on a database that already has an active
	// version it does nothing, so a service that has published edits does not get its
	// corpus stamped back to the bundled one on the next restart.
	//
	// Not re-embedding is the load-bearing part. corpus/faq.json is byte-identical to the
	// Java implementation's, and its vectors are what every retrieval number in this pair
	// was measured against; recomputing them would move the measurement while claiming to
	// preserve it.
	corpus, err := rag.LoadCorpus(cfg.RAG.CorpusPath)
	if err != nil {
		return err
	}
	if adopted, err := vectors.AdoptBundled(startupCtx, corpus.Version); err != nil {
		return fmt.Errorf("adopt the bundled corpus: %w", err)
	} else if adopted {
		slog.Info("adopted the bundled corpus as the first managed version",
			"version", corpus.Version)
	}
	// The drafts operators edit. Seeded from the bundled corpus once, because an editor
	// that opens on an empty list turns the first publication into "replace the knowledge
	// base with whatever one person just typed" -- and nobody would find out until
	// customers stopped being answered.
	knowledgeStore := knowledge.NewStore(pool, vectors, embedder)
	if seeded, err := knowledgeStore.SeedFromCorpus(startupCtx, corpus); err != nil {
		return fmt.Errorf("seed the knowledge base: %w", err)
	} else if seeded > 0 {
		slog.Info("seeded the editable knowledge base from the bundled corpus",
			"entries", seeded)
	}

	if active, _, err := vectors.Active(startupCtx); err == nil {
		slog.Info("retrieval reads one corpus version", "active", active)
	} else if errors.Is(err, rag.ErrNoActiveVersion) {
		// Reachable with IngestOnStartup off against an empty database, and the warning is
		// the only thing that tells anyone why every answer is ungrounded.
		slog.Warn("no active corpus version: retrieval will fall back to unversioned documents")
	} else {
		return err
	}

	client, err := llm.New(cfg.Chat)
	if err != nil {
		return err
	}

	tickets := ticket.NewStore(pool)

	// The loop back to a human, and back from them.
	//
	// The webhook is optional and its absence is a working no-op, but an absent
	// destination means a raised ticket tells nobody: it is the difference between an
	// assistant that escalates and one that files into a drawer, and it says so.
	notifier := handoff.NewNotifier(pool, cfg.Handoff.WebhookURL, cfg.Handoff.Timeout).Meter(metrics)
	handoffs := handoff.NewStore(pool, chat.NewMemory(pool, 40), notifier)
	if notifier.Enabled() {
		slog.Info("handoff notifications enabled", "timeout", cfg.Handoff.Timeout)
	} else {
		slog.Warn("HANDOFF_WEBHOOK_URL is unset: a raised ticket notifies nobody. " +
			"Operators still see tickets in the operations UI, and nothing tells them to look.")
	}

	// Where lookup_order_status reads from, and the start-up line that says which.
	//
	// The failure being prevented is not an outage. It is a service answering from five
	// hard-coded orders while everyone -- the operator watching the dashboard, the person
	// reading the demo, the next session -- believes it is talking to the order system.
	// That state is indistinguishable from working, so the only thing that can reveal it
	// is the service saying so out loud, every time it starts.
	orders, err := orderSource(cfg.Orders)
	if err != nil {
		return err
	}

	service := chat.NewService(
		chat.NewMemory(pool, cfg.Chat.MaxHistoryMessages),
		rag.NewRetriever(embedder, vectors, cfg.RAG.TopK, cfg.RAG.SimilarityThreshold),
		client,
		cost.NewBudget(cfg.Cost.ConversationTokenBudget, cfg.Cost.TrackedConversations),
		metrics,
		chat.NewRecorder(pool),
		cfg.Chat.MaxTokens,
		tools.NewOrderLookup(orders),
		tools.NewSupportTickets(tickets, handoffs),
	)

	// Who may talk to the chat endpoints, and whose conversation is whose.
	//
	// AUTH_MODE=off is the pre-identity behaviour: client-supplied conversation ids, no
	// ownership, anyone who knows an id can append to that history. It is what the
	// benchmark and the cross-repository comparison drive, and it is not a production
	// configuration -- so it says so at every start-up rather than in a document.
	mode, err := identity.ParseMode(cfg.Auth.Mode)
	if err != nil {
		return err
	}
	var ident *httpapi.Identity
	if mode == identity.ModeSession {
		limits := identity.NewLimits(pool)
		limits.TurnsPerMinute = cfg.Auth.TurnsPerMinute
		limits.SessionsPerHourPerIP = cfg.Auth.SessionsPerHourPerIP
		limits.DailyTokenBudget = cfg.Auth.DailyTokenBudget
		ident = &httpapi.Identity{
			Sessions:      identity.NewSessions(pool, cfg.Auth.SessionTTL),
			Conversations: identity.NewConversations(pool),
			Limits:        limits,
		}
		slog.Info("chat sessions are required",
			"session_ttl", cfg.Auth.SessionTTL,
			"turns_per_minute", cfg.Auth.TurnsPerMinute,
			"sessions_per_hour_per_ip", cfg.Auth.SessionsPerHourPerIP,
			"daily_token_budget", cfg.Auth.DailyTokenBudget)
	} else {
		slog.Warn("AUTH_MODE=off: the chat endpoints are unauthenticated and a conversation " +
			"id is the whole of the authorisation. Anyone who knows one can append to that " +
			"history. Use AUTH_MODE=session outside a benchmark or a demo.")
	}

	mux := http.NewServeMux()
	httpapi.NewServer(service, cfg.Chat, ident, handoffs).Routes(mux)

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
			admin.ParseCORS(cfg.Admin.CORSOrigins), retention.NewStore(pool), handoffs,
			knowledgeStore, feedback.NewStore(pool)).Routes(mux)
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

	// Deleting what is too old to keep.
	//
	// Off by default, and it says so, because "we never deleted anything" is not a
	// position anyone means to hold -- it is one they discover when somebody asks.
	if cfg.Retention.Window > 0 {
		slog.Info("retention sweeper started",
			"window", cfg.Retention.Window, "interval", cfg.Retention.SweepInterval)
		go retention.NewSweeper(retention.NewStore(pool),
			cfg.Retention.Window, cfg.Retention.SweepInterval).Run(ctx)
	} else {
		slog.Warn("RETENTION_DAYS is unset: conversations and their text are kept for ever. " +
			"Erasure on request is available to operators; expiry by age is not running.")
	}

	// A turn whose process died stays in flight for ever otherwise, and the overview counts
	// it under "not completed" as though the customer had walked away. A crash and a closed
	// tab are the two things that record exists to tell apart.
	go func() {
		recorder := chat.NewRecorder(pool)
		ticker := time.NewTicker(cfg.Chat.TurnLease)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := recorder.Sweep(ctx, cfg.Chat.TurnLease)
				if err != nil {
					slog.Error("could not sweep abandoned turns", "error", err)
					continue
				}
				if n > 0 {
					// Only when it found something. An hourly line saying "nothing" is
					// how a log stops being read.
					slog.Warn("marked turns abandoned by a stopped process",
						"turns", n, "lease", cfg.Chat.TurnLease)
				}
			}
		}
	}()

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
