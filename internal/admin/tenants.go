package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lai3d/ai-customer-service-go/internal/tenant"
)

// The tenant administration surface, for the platform role only.
//
// Nothing here reads customer content. A tenant's id, name and keys say who a *customer of
// this service* is; they never say what that customer's own customers wrote. That is the
// separation the role exists for: the account with the widest reach in the system is the
// one holding the least.
//
// Every action is audited like every other operator action, and the audit row carries the
// tenant being administered rather than the operator's (which is empty by construction).

// Tenants is what this surface needs from internal/tenant. An interface for the same reason
// every other seam here is one: the handler's job is the guard and the audit row, and it
// should be testable without deciding how a key is hashed.
type Tenants interface {
	List(ctx context.Context) ([]tenant.Tenant, error)
	Create(ctx context.Context, id, name, actor string) (tenant.Tenant, error)
	Get(ctx context.Context, id string) (tenant.Tenant, error)
	SetDisabled(ctx context.Context, id string, disabled bool) error
	Keys(ctx context.Context, tenantID string) ([]tenant.Key, error)
	IssueKey(ctx context.Context, tenantID, label, actor string) (tenant.Key, error)
	RevokeKey(ctx context.Context, keyID string) error
}

func (s *Server) tenantList(w http.ResponseWriter, r *http.Request) {
	list, err := s.tenants.List(r.Context())
	if err != nil {
		fail(w, r, "tenants", err)
		return
	}
	writeJSON(w, map[string]any{"tenants": list})
}

func (s *Server) tenantCreate(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	made, err := s.tenants.Create(r.Context(), body.ID, body.Name, operator.Name)
	switch {
	case errors.Is(err, tenant.ErrExists):
		s.recordFor(r, body.ID, "create tenant", "tenant/"+body.ID, "conflict", "")
		http.Error(w, "that tenant id is already taken", http.StatusConflict)
		return
	case err != nil:
		s.recordFor(r, body.ID, "create tenant", "tenant/"+body.ID, "rejected", err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	s.recordFor(r, made.ID, "create tenant", "tenant/"+made.ID, "ok", made.Name)
	writeJSON(w, made)
}

func (s *Server) tenantSetDisabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	switch err := s.tenants.SetDisabled(r.Context(), id, body.Disabled); {
	case errors.Is(err, tenant.ErrNoSuchTenant):
		http.Error(w, "no such tenant", http.StatusNotFound)
		return
	case err != nil:
		fail(w, r, "tenants", err)
		return
	}
	outcome := "enabled"
	if body.Disabled {
		outcome = "disabled"
	}
	// Audited loudly: disabling a tenant stops every one of its keys answering, which is
	// an outage for somebody, and it is reversible only by whoever notices.
	s.recordFor(r, id, "set tenant disabled", "tenant/"+id, "ok", outcome)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tenantKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.tenants.Keys(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, r, "tenants", err)
		return
	}
	// No secret is in here and none can be: it is not stored. Listing keys is a read of
	// which credentials exist, not of the credentials.
	writeJSON(w, map[string]any{"keys": keys})
}

func (s *Server) tenantIssueKey(w http.ResponseWriter, r *http.Request) {
	operator, _ := FromContext(r.Context())
	id := r.PathValue("id")
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "the request body is not valid JSON", http.StatusBadRequest)
		return
	}
	key, err := s.tenants.IssueKey(r.Context(), id, body.Label, operator.Name)
	switch {
	case errors.Is(err, tenant.ErrNoSuchTenant):
		http.Error(w, "no such tenant", http.StatusNotFound)
		return
	case err != nil:
		s.recordFor(r, id, "issue tenant key", "tenant/"+id, "rejected", err.Error())
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	// The key_id is in the audit row and the secret is not. Which key was issued, to whom
	// and when is the question asked after an incident; the secret is the thing the
	// incident would be about.
	s.recordFor(r, id, "issue tenant key", "tenant/"+id+"/key/"+key.KeyID, "ok", key.Label)
	// Never cached, anywhere, by anything: this response body is a credential and it is
	// the only time it exists outside the caller.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, key)
}

func (s *Server) tenantRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, keyID := r.PathValue("id"), r.PathValue("keyId")
	// Checked against the tenant in the path rather than revoked by key_id alone, so a
	// key_id from one tenant cannot be revoked through another's URL -- the same shape of
	// mistake as reading a ticket by number.
	keys, err := s.tenants.Keys(r.Context(), id)
	if err != nil {
		fail(w, r, "tenants", err)
		return
	}
	var theirs bool
	for _, k := range keys {
		if k.KeyID == keyID {
			theirs = true
		}
	}
	if !theirs {
		http.Error(w, "no such key for that tenant", http.StatusNotFound)
		return
	}
	switch err := s.tenants.RevokeKey(r.Context(), keyID); {
	case errors.Is(err, tenant.ErrNoSuchKey):
		http.Error(w, "that key is already revoked", http.StatusNotFound)
		return
	case err != nil:
		fail(w, r, "tenants", err)
		return
	}
	s.recordFor(r, id, "revoke tenant key", "tenant/"+id+"/key/"+keyID, "ok", "")
	w.WriteHeader(http.StatusNoContent)
}
