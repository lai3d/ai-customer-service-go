package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lai3d/ai-customer-service-go/internal/tenant"
)

// TenantKeyHeader is where the tenant's API key arrives.
//
// Its own header rather than `Authorization`, which already carries the customer's session
// token. They are two different identities — *which product this is* and *which visitor
// this is* — and the caller is a server for the first and a browser for the second. One
// header carrying whichever happened to be sent is how a client ends up authenticating as
// the wrong thing and nobody notices until it works.
const TenantKeyHeader = "X-API-Key"

// Tenants is the resolver. An interface so the edge can be tested with something that
// cannot also be answering from a row it wrote.
type Tenants interface {
	Resolve(ctx context.Context, presented string) (string, error)
}

// tenantFor decides which tenant a request is for, before anything else happens to it.
//
// Three cases, and the middle one is the one worth being careful about:
//
//   - a key is presented: it is resolved, and an invalid one is 401. There is no mode in
//     which a wrong key quietly becomes the default tenant — that is the failure where a
//     misconfigured integration writes another customer's data into `default` and every
//     request looks successful.
//   - no key, TENANCY=single: the default tenant. What this service has always done, and
//     what the benchmark and the cross-repository parity fixtures run as.
//   - no key, TENANCY=required: 401, before any model call.
func (s *Server) tenantFor(r *http.Request) (string, *problem) {
	presented := strings.TrimSpace(r.Header.Get(TenantKeyHeader))

	if presented == "" {
		if s.identity != nil && s.identity.RequireKey {
			s.metrics.Refusals.WithLabelValues("no_tenant").Inc()
			return "", &problem{
				Title:  "No API key",
				Status: http.StatusUnauthorized,
				Detail: "Send this service's API key in the " + TenantKeyHeader + " header.",
			}
		}
		return tenant.Default, nil
	}

	if s.identity == nil || s.identity.Tenants == nil {
		// A key was sent to a service that cannot check one. Refusing is the only honest
		// answer: accepting it would mean the caller believes it is one tenant and the
		// service believes it is another, which is worse than either being wrong alone.
		s.metrics.Refusals.WithLabelValues("no_tenant").Inc()
		return "", &problem{Title: "API keys are not enabled", Status: http.StatusUnauthorized,
			Detail: "This service is not configured for more than one tenant."}
	}

	id, err := s.identity.Tenants.Resolve(r.Context(), presented)
	switch {
	case errors.Is(err, tenant.ErrNoSuchKey):
		s.metrics.Refusals.WithLabelValues("no_tenant").Inc()
		// The log says nothing about the key. A key_id in a log line is a key_id an
		// attacker who can read logs no longer has to guess, and the client has been told
		// everything it is allowed to know.
		slog.Warn("a request presented an API key that does not resolve",
			"path", r.URL.Path, "remote", clientIP(r))
		return "", &problem{Title: "Unknown API key", Status: http.StatusUnauthorized,
			Detail: "The API key is not valid for this service."}
	case err != nil:
		// A database failure is not a bad key, and answering 401 would tell a working
		// integration to go and rotate a key that is fine.
		return "", &problem{Title: "Could not check the API key",
			Status: http.StatusServiceUnavailable,
			Detail: "Retrying shortly is worthwhile."}
	}
	return id, nil
}
