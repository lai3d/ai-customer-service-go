// Package config loads every tunable from the environment, in one place, with the
// reasoning for each default written next to it.
//
// Defaults are chosen for an interactive service. Several of the numbers here are
// measurements rather than taste, and the comments say which.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration

	Postgres Postgres
	Chat     Chat
	RAG      RAG
	Cost     Cost
	Obs      Obs
	Admin    Admin
	Auth     Auth
}

type Postgres struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	// A pool this size is not a throughput setting so much as a bound on how much
	// concurrency reaches Postgres. Raising it to 100 under the benchmark bought about
	// 7% in the Java implementation, so it is not where the time goes.
	MaxConns int32
}

// URL builds the DSN through net/url rather than by formatting a string.
//
// A password containing any of / ? # @ : -- all of which are legal in a Postgres
// password and common in generated ones -- turns a formatted DSN into a different URL, or
// into one that will not parse at all: `test/a#b%` fails with "invalid port after host"
// before a connection is ever attempted. url.UserPassword escapes the userinfo, and the
// database name goes in as a path segment rather than as text.
func (p Postgres) URL() string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(p.User, p.Password),
		Host:     net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Path:     "/" + p.Database,
		RawQuery: url.Values{"sslmode": {"disable"}}.Encode(),
	}
	return u.String()
}

type Chat struct {
	// Provider selects the chat backend: anthropic, openai, xai, gemini.
	Provider string
	Model    string
	APIKey   string
	BaseURL  string

	MaxTokens int

	// How many messages of history travel with each request. Every one is re-sent and
	// re-billed on every turn, so this is a cost lever, not just a memory setting.
	MaxHistoryMessages int

	// A conversation id is stored in a bounded column and echoed in a header; an
	// unvalidated, unbounded id from a client turned into a 500 in the Java version.
	MaxConversationIDLength int
	MaxMessageLength        int

	// Interactive retry. Library defaults are chosen for batch jobs: Spring AI's were
	// 10 attempts with a 180s cap, which is 1142 seconds of backoff before the customer
	// is told it failed. Three attempts with a 1s/2s gap cap the added wait at 3s.
	RetryMaxAttempts int
	RetryInitial     time.Duration
	RetryMultiplier  float64
	RetryMax         time.Duration

	// Guards against a stall, not against slowness: a long answer legitimately takes
	// time. Go's http.Client ships no timeout at all by default, which is the same
	// hazard Spring Boot had.
	ConnectTimeout time.Duration
	RequestTimeout time.Duration

	// SSE connections are legitimately idle between the request and the first token,
	// and proxies close idle connections.
	KeepAliveInterval time.Duration
}

type RAG struct {
	CorpusPath      string
	IngestOnStartup bool

	ModelPath     string
	TokenizerPath string
	Dimensions    int

	// Passages per question, measured rather than guessed: see docs/retrieval.md.
	TopK int
	// Zero, and that is a measurement rather than an omission.
	//
	// With multilingual-e5-small the scores of relevant questions, off-topic questions
	// and degenerate input all overlap: the weakest real match scores 0.8378, the
	// strongest off-topic question 0.8490, and three Chinese full stops 0.8417. No value
	// filters relevance, and none works as a floor for junk either. Relevance judgement
	// lives in the system prompt instead. The knob stays because a different embedding
	// model may well have a usable distribution -- re-measure before setting it.
	// TestNoSimilarityThresholdIsUseful holds the measurement.
	SimilarityThreshold float64

	// e5 is trained with asymmetric input markers. They are part of the model contract;
	// applying them to one side only is worse than applying neither.
	QueryPrefix   string
	PassagePrefix string

	// How many goroutines may be inside the native embedding call at once.
	//
	// 0 means GOMAXPROCS, which is the measured default: a goroutine blocked in cgo
	// blocks its OS thread and the Go scheduler creates another, so a thousand
	// simultaneous arrivals took the process to 146-276 OS threads. Bounding holds it
	// at 40 for about 8% of throughput. The work is CPU-bound, so admitting more
	// goroutines than cores buys threads and nothing else. See docs/benchmark.md.
	MaxConcurrentEmbeddings int
}

type Cost struct {
	// A conversation is an open-ended bill unless capped: a message window bounds any
	// single request, nothing bounds the number of requests. 0 disables the cap.
	ConversationTokenBudget int64
	TrackedConversations    int
}

// Admin configures the operations surface. Empty means it is not mounted at all.
type Admin struct {
	// Tokens is `name:token[:role]`, comma separated. Parsed by internal/admin, not
	// here, because the parse is the security check and belongs with the thing it
	// guards.
	Tokens string
	// CORSOrigins is the comma-separated list of origins the operations UI is served
	// from. Empty disables CORS, which is right when a reverse proxy puts the UI and
	// this API on one origin, and wrong the moment they are two containers.
	CORSOrigins string
}

// Auth configures who may talk to the chat endpoints. See internal/identity.
type Auth struct {
	// Mode is "off" or "session". Off keeps the pre-identity behaviour -- client-supplied
	// conversation ids and no ownership -- which the benchmark and the cross-repository
	// comparison depend on, and which is not a production configuration.
	Mode string
	// SessionTTL is how long an issued session stays valid.
	SessionTTL time.Duration
	// TurnsPerMinute bounds one subject. Zero disables it.
	TurnsPerMinute int
	// SessionsPerHourPerIP bounds the endpoint that mints subjects. Zero disables it.
	SessionsPerHourPerIP int
	// DailyTokenBudget is the whole service's ceiling for a UTC day. Zero disables it.
	//
	// This is the one the per-conversation budget cannot be: conversation ids are free,
	// so a per-conversation ceiling is a ceiling on politeness.
	DailyTokenBudget int64
}

type Obs struct {
	OTLPEndpoint  string
	OTLPEnabled   bool
	TraceSampling float64
	// Spring AI attached the customer's query to every vector-store span with no
	// property to disable it. Nothing here does that by default; this switch exists so
	// the choice is deliberate rather than accidental.
	IncludeQueryContent bool
}

func Load() (Config, error) {
	c := Config{
		// 8081, not 8080. The Java implementation of this system uses 8080, both stacks
		// are expected to run on one machine, and every document here -- README, the
		// container, the healthcheck -- says 8081. The source default said 8080 and
		// only the Dockerfile's explicit override hid it.
		HTTPAddr:        env("HTTP_ADDR", ":8081"),
		ShutdownTimeout: envDuration("SHUTDOWN_GRACE", 30*time.Second),
		Postgres: Postgres{
			Host:     env("POSTGRES_HOST", "localhost"),
			Port:     envInt("POSTGRES_PORT", 5432),
			Database: env("POSTGRES_DB", "csagent"),
			User:     env("POSTGRES_USER", "csagent"),
			Password: env("POSTGRES_PASSWORD", "csagent"),
			MaxConns: int32(envInt("POSTGRES_MAX_CONNS", 20)),
		},
		Chat: Chat{
			Provider:                strings.ToLower(env("CHAT_PROVIDER", "anthropic")),
			MaxTokens:               envInt("CHAT_MAX_TOKENS", 8192),
			MaxHistoryMessages:      envInt("CHAT_MAX_HISTORY_MESSAGES", 40),
			MaxConversationIDLength: 64,
			MaxMessageLength:        envInt("CHAT_MAX_MESSAGE_LENGTH", 4000),
			RetryMaxAttempts:        envInt("AI_RETRY_MAX_ATTEMPTS", 3),
			RetryInitial:            time.Second,
			RetryMultiplier:         2,
			RetryMax:                10 * time.Second,
			ConnectTimeout:          envDuration("HTTP_CONNECT_TIMEOUT", 10*time.Second),
			RequestTimeout:          envDuration("HTTP_READ_TIMEOUT", 120*time.Second),
			KeepAliveInterval:       envDuration("SSE_KEEPALIVE", 15*time.Second),
		},
		RAG: RAG{
			CorpusPath:              env("FAQ_CORPUS_PATH", "corpus/faq.json"),
			IngestOnStartup:         envBool("FAQ_INGEST_ON_STARTUP", true),
			ModelPath:               env("EMBEDDING_MODEL_PATH", "model-cache/multilingual-e5-small/model.onnx"),
			TokenizerPath:           env("EMBEDDING_TOKENIZER_PATH", "model-cache/multilingual-e5-small/tokenizer.json"),
			Dimensions:              envInt("EMBEDDING_DIMENSIONS", 384),
			TopK:                    envInt("RAG_TOP_K", 8),
			SimilarityThreshold:     envFloat("RAG_SIMILARITY_THRESHOLD", 0),
			QueryPrefix:             env("EMBEDDING_QUERY_PREFIX", "query: "),
			PassagePrefix:           env("EMBEDDING_PASSAGE_PREFIX", "passage: "),
			MaxConcurrentEmbeddings: envInt("EMBEDDING_MAX_CONCURRENCY", 0),
		},
		Cost: Cost{
			ConversationTokenBudget: int64(envInt("CONVERSATION_TOKEN_BUDGET", 200_000)),
			TrackedConversations:    envInt("TRACKED_CONVERSATIONS", 10_000),
		},
		Admin: Admin{
			// Deliberately no default. The operations surface shows customer
			// conversations, which every other part of this service takes trouble to
			// keep out of logs, spans and metric labels; it should exist only where
			// somebody decided it should.
			Tokens: os.Getenv("ADMIN_TOKENS"),
			// Also deliberately no default: "*" is not reachable from configuration,
			// and localhost is not assumed.
			CORSOrigins: os.Getenv("ADMIN_CORS_ORIGINS"),
		},
		Auth: Auth{
			// Deliberately off by default: turning identity on changes what the
			// benchmark measures, and a default that silently changes a measurement is
			// worse than one that has to be chosen.
			Mode:                 env("AUTH_MODE", "off"),
			SessionTTL:           time.Duration(envInt("SESSION_TTL_HOURS", 24)) * time.Hour,
			TurnsPerMinute:       envInt("TURNS_PER_MINUTE", 20),
			SessionsPerHourPerIP: envInt("SESSIONS_PER_HOUR_PER_IP", 60),
			DailyTokenBudget:     int64(envInt("DAILY_TOKEN_BUDGET", 0)),
		},
		Obs: Obs{
			OTLPEndpoint:        env("OTLP_TRACING_ENDPOINT", "http://localhost:4318"),
			OTLPEnabled:         envBool("OTLP_TRACING_EXPORT_ENABLED", false),
			TraceSampling:       envFloat("TRACING_SAMPLE_RATE", 1.0),
			IncludeQueryContent: envBool("TRACE_INCLUDE_QUERY_CONTENT", false),
		},
	}

	provider, err := resolveProvider(c.Chat.Provider)
	if err != nil {
		return Config{}, err
	}
	c.Chat.Model = provider.model
	c.Chat.APIKey = provider.apiKey
	c.Chat.BaseURL = provider.baseURL

	if c.Chat.APIKey == "" {
		// A missing key must fail startup, not every request. Reporting healthy and
		// then 401ing every customer is the worse failure: Kubernetes marks the pod
		// ready and routes traffic to it.
		return Config{}, fmt.Errorf("chat provider %q selected but %s is not set",
			c.Chat.Provider, provider.keyVar)
	}

	// A non-positive heartbeat panics time.NewTicker inside the SSE handler, so it takes
	// down a connection mid-response for every streamed turn rather than failing here
	// once with the name of the variable that is wrong.
	if c.Chat.KeepAliveInterval <= 0 {
		return Config{}, fmt.Errorf("SSE_KEEPALIVE must be positive, got %s",
			c.Chat.KeepAliveInterval)
	}
	if _, err := identityMode(c.Auth.Mode); err != nil {
		return Config{}, err
	}
	return c, nil
}

// identityMode duplicates the parse in internal/identity so that a misspelt AUTH_MODE
// fails at start-up with the rest of the configuration rather than several lines later.
func identityMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "", "off":
		return "off", nil
	case "session":
		return "session", nil
	default:
		return "", fmt.Errorf("AUTH_MODE %q is not off or session", mode)
	}
}

type providerDefaults struct {
	model, apiKey, baseURL, keyVar string
}

func resolveProvider(name string) (providerDefaults, error) {
	switch name {
	case "anthropic":
		return providerDefaults{
			// Sampling parameters are deliberately absent everywhere: Claude Opus 5
			// returns HTTP 400 for temperature, top_p or top_k, and GPT-5 accepts only
			// its own default. Nothing in this codebase sets one.
			model:   env("ANTHROPIC_CHAT_MODEL", "claude-opus-5"),
			apiKey:  os.Getenv("ANTHROPIC_API_KEY"),
			baseURL: env("ANTHROPIC_BASE_URL", ""),
			keyVar:  "ANTHROPIC_API_KEY",
		}, nil
	case "openai":
		return providerDefaults{
			model:   env("OPENAI_CHAT_MODEL", "gpt-5"),
			apiKey:  os.Getenv("OPENAI_API_KEY"),
			baseURL: env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			keyVar:  "OPENAI_API_KEY",
		}, nil
	case "xai":
		// A separate provider reached over a shared protocol. Putting an xAI key in
		// OPENAI_API_KEY with a base-URL override works and lies: the configuration
		// then says OpenAI everywhere while talking to xAI.
		return providerDefaults{
			model:   env("XAI_CHAT_MODEL", "grok-4.6"),
			apiKey:  os.Getenv("XAI_API_KEY"),
			baseURL: env("XAI_BASE_URL", "https://api.x.ai/v1"),
			keyVar:  "XAI_API_KEY",
		}, nil
	default:
		// Gemini is not here on purpose. A provider that config accepts and the client
		// layer cannot build would fail later and less clearly than this does.
		return providerDefaults{}, fmt.Errorf(
			"unknown CHAT_PROVIDER %q: want anthropic, openai or xai", name)
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
