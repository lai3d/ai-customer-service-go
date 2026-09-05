// Package admin is the operations surface: the conversations, the tickets and the audit
// trail behind them.
//
// Everything else in this service takes care to keep customer text out of the places it
// leaks to -- no query content on spans, no customer words in metric labels, nothing
// sensitive in a log line. This package is the deliberate exception: an operator working
// a ticket has to read what the customer wrote. That makes it the one surface where all
// of that care is concentrated, which is why it does not exist at all unless someone has
// configured who may use it.
package admin

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

type Role string

const (
	// RoleViewer may read conversations and tickets. RoleOperator may also change them.
	// Two roles, because this release has two kinds of action; a permission model with
	// more entries than the actions it governs is a design document, not a control.
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
)

type Operator struct {
	Name string
	Role Role
	// token is never logged, never returned, and never compared with ==.
	token string
}

func (o Operator) CanWrite() bool { return o.Role == RoleOperator }

// Operators is the configured set. An empty set means the admin surface is not mounted
// at all -- see Enabled.
type Operators struct{ byName []Operator }

// ParseOperators reads `name:token[:role]` entries, comma separated.
//
//	ADMIN_TOKENS="alex:s3cret:operator,dana:othersecret"
//
// The name is not decoration. An audit trail whose every entry says "admin" answers when
// something happened and not who did it, which is most of the point of having one. A
// missing role means viewer: least privilege is the safe direction for a typo.
func ParseOperators(spec string) (Operators, error) {
	var ops Operators
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return Operators{}, fmt.Errorf("ADMIN_TOKENS entry %d is not name:token[:role]",
				len(ops.byName)+1)
		}
		role := RoleViewer
		if len(parts) > 2 {
			switch Role(parts[2]) {
			case RoleOperator:
				role = RoleOperator
			case RoleViewer, "":
				role = RoleViewer
			default:
				return Operators{}, fmt.Errorf("operator %q has unknown role %q: want %s or %s",
					parts[0], parts[2], RoleViewer, RoleOperator)
			}
		}
		if len(parts[1]) < 16 {
			return Operators{}, fmt.Errorf("the token for %q is %d characters; this is the "+
				"credential for every customer conversation in the database, so 16 is the "+
				"minimum and a generated one is better", parts[0], len(parts[1]))
		}
		ops.byName = append(ops.byName, Operator{Name: parts[0], Role: role, token: parts[1]})
	}
	return ops, nil
}

// Enabled reports whether any operator is configured. When false the routes are never
// registered: the admin surface returns 404 because it does not exist, not 401 because
// it is guarded. A guard that can be misconfigured is a guard that can be absent.
func (o Operators) Enabled() bool { return len(o.byName) > 0 }

func (o Operators) Names() []string {
	names := make([]string, 0, len(o.byName))
	for _, op := range o.byName {
		names = append(names, op.Name+" ("+string(op.Role)+")")
	}
	return names
}

// lookup compares every configured token regardless of when it matches, so the time this
// takes does not depend on which operator presented a credential or on how much of a
// wrong one was correct.
func (o Operators) lookup(token string) (Operator, bool) {
	var found Operator
	var ok bool
	for _, op := range o.byName {
		if subtle.ConstantTimeCompare([]byte(op.token), []byte(token)) == 1 {
			found, ok = op, true
		}
	}
	return found, ok
}

type contextKey struct{}

// FromContext returns the operator a request was authenticated as.
func FromContext(ctx context.Context) (Operator, bool) {
	op, ok := ctx.Value(contextKey{}).(Operator)
	return op, ok
}

// Authenticate rejects anything without a recognised bearer token.
func (o Operators) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			// Sent so a browser can prompt, and so a misconfigured client gets a
			// diagnosis rather than a blank 401.
			w.Header().Set("WWW-Authenticate", `Bearer realm="operations"`)
			http.Error(w, "an operator bearer token is required", http.StatusUnauthorized)
			return
		}
		operator, found := o.lookup(strings.TrimSpace(token))
		if !found {
			http.Error(w, "unknown operator token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, operator)))
	})
}

// Refusal is called when an authenticated operator is refused an action. A rejected
// attempt is exactly what an audit trail is for -- "who tried to do what they may not" is
// more interesting than most of what succeeds -- and it was missing until a live walk
// through the workflow showed a 403 leaving no trace at all.
//
// An unauthenticated request is not passed here: there is no operator to attribute it to,
// so it belongs in the service log rather than in a table of who did what.
type Refusal func(r *http.Request, operator Operator)

// RequireWrite gates the mutating handlers. Checked on the server for every request,
// because hiding a button is a user-interface decision and not an access control.
func RequireWrite(refused Refusal, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operator, ok := FromContext(r.Context())
		if !ok || !operator.CanWrite() {
			if ok && refused != nil {
				refused(r, operator)
			}
			http.Error(w, "this operator may read but not change", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
