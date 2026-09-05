package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORS is the allowlist of origins the operations UI may be served from.
//
// The UI is a separate application on a separate origin now, so the browser will not let
// it call this API without these headers. That makes the allowlist a security control
// rather than plumbing: what it permits is *other pages reading this API's responses*,
// and those responses are every conversation a customer has had.
//
// Three rules follow, and each one is a way this is commonly got wrong:
//
//   - Never `*`. It is the value that makes the error class disappear during development
//     and reappear as "any page on the internet can read the support inbox" in
//     production. There is no configuration here that produces it.
//   - The origin is echoed only if it matched, and `Vary: Origin` always goes with it.
//     Without `Vary`, a shared cache can hand one origin's allowed response to another,
//     which is the same hole arrived at by accident.
//   - A preflight is answered before authentication. Browsers do not put an
//     `Authorization` header on an `OPTIONS` preflight, so an authenticating preflight
//     rejects every cross-origin request the moment the UI sends its token -- and the
//     failure surfaces as an opaque network error with nothing in the server log.
type CORS struct{ origins []string }

// ParseCORS reads a comma-separated origin list. An empty list disables CORS entirely --
// no headers, no preflight -- which is correct for a same-origin deployment behind one
// reverse proxy, and is why this is not defaulted to something permissive.
func ParseCORS(spec string) CORS {
	var c CORS
	for _, o := range strings.Split(spec, ",") {
		if o = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(o), "/")); o != "" {
			c.origins = append(c.origins, o)
		}
	}
	return c
}

func (c CORS) Enabled() bool { return len(c.origins) > 0 }

// allows compares whole origins, never prefixes. A prefix match on
// "https://ops.example.com" also accepts "https://ops.example.com.evil.test".
func (c CORS) allows(origin string) bool {
	for _, o := range c.origins {
		if o == origin {
			return true
		}
	}
	return false
}

const preflightMaxAge = 10 * time.Minute

// Wrap answers preflights and adds the response headers. It wraps the whole admin API,
// outside authentication.
func (c CORS) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Vary goes on every response that could have carried the header, including
			// the ones where the origin did not match, or a cache keyed on the request
			// without Origin will serve a header meant for someone else.
			w.Header().Add("Vary", "Origin")
		}
		allowed := origin != "" && c.allows(origin)
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			if !allowed {
				// No CORS headers, and a status the browser will refuse anyway. Saying
				// 403 rather than 204-with-nothing gives an operator debugging a
				// misconfigured origin something to see in the network pane.
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(preflightMaxAge.Seconds())))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
