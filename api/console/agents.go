package console

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/oauth"
)

const agentsPage = "/cb/agents"

// Agents checks and answers an agent's authorization request, and lists, revokes and acknowledges a user's grants.
type Agents interface {
	Check(ctx context.Context, user string, req oauth.Request) (oauth.Consent, error)
	Approve(ctx context.Context, user string, req oauth.Request, caps oauth.Caps) (string, error)
	Deny(ctx context.Context, req oauth.Request) (string, error)
	Grants(ctx context.Context, user string) ([]oauth.Grant, error)
	Revoke(ctx context.Context, user, id string) error
	MarkSeen(ctx context.Context, user string) error
}

// EnableAgents serves the consent page of oauth.AuthorizePath, the Agents page and their calls over agents.
func (s *Server) EnableAgents(agents Agents) {
	s.agents = agents
	s.mux.HandleFunc("GET "+oauth.AuthorizePath, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/authorize.html")
	})
	s.mux.HandleFunc("GET "+agentsPage, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/agents.html")
	})
	s.mux.HandleFunc("POST "+Prefix+"api/oauth/check", s.signedIn(s.checkAuthorization))
	s.mux.HandleFunc("POST "+Prefix+"api/oauth/approve", s.signedIn(s.approveAuthorization))
	s.mux.HandleFunc("POST "+Prefix+"api/oauth/deny", s.signedIn(s.denyAuthorization))
	s.mux.HandleFunc("GET "+Prefix+"api/agents", s.signedIn(s.listAgents))
	s.mux.HandleFunc("POST "+Prefix+"api/agents/seen", s.signedIn(s.markAgentsSeen))
	s.mux.HandleFunc("DELETE "+Prefix+"api/agents/{id}", s.signedIn(s.revokeAgent))
}

func (s *Server) checkAuthorization(w http.ResponseWriter, r *http.Request) {
	req, _, ok := decodeAuthorization(w, r)
	if !ok {
		return
	}
	u := caller(r)
	consent, err := s.agents.Check(r.Context(), u.ID, req)
	if err != nil {
		s.failAuthorization(w, r, err)
		return
	}
	list, err := s.boxes.List(s.ownerCtx(r), &controlv1.ListBoxesRequest{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, struct {
		oauth.Consent
		User         string `json:"user"`
		AccountBoxes int    `json:"account_boxes"`
		AccountLimit int    `json:"account_max_boxes"`
	}{Consent: consent, User: u.Name, AccountBoxes: len(list.GetBoxes()), AccountLimit: u.MaxBoxes})
}

func (s *Server) approveAuthorization(w http.ResponseWriter, r *http.Request) {
	req, caps, ok := decodeAuthorization(w, r)
	if !ok {
		return
	}
	redirect, err := s.agents.Approve(r.Context(), caller(r).ID, req, caps)
	if err != nil {
		s.failAuthorization(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"redirect": redirect})
}

func (s *Server) denyAuthorization(w http.ResponseWriter, r *http.Request) {
	req, _, ok := decodeAuthorization(w, r)
	if !ok {
		return
	}
	redirect, err := s.agents.Deny(r.Context(), req)
	if err != nil {
		s.failAuthorization(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"redirect": redirect})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	grants, err := s.agents.Grants(r.Context(), caller(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]oauth.Grant{}, grants...))
}

func (s *Server) markAgentsSeen(w http.ResponseWriter, r *http.Request) {
	if err := s.agents.MarkSeen(r.Context(), caller(r).ID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeAgent(w http.ResponseWriter, r *http.Request) {
	err := s.agents.Revoke(r.Context(), caller(r).ID, r.PathValue("id"))
	switch {
	case errors.Is(err, oauth.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.fail(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// failAuthorization answers a refused request with its message and, when the client may hear it, where to send the browser.
func (s *Server) failAuthorization(w http.ResponseWriter, r *http.Request, err error) {
	refusal, refused := errors.AsType[*oauth.AuthorizeError](err)
	switch {
	case refused:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.MarshalWrite(w, struct {
			Error    string `json:"error"`
			Redirect string `json:"redirect"`
		}{refusal.Description, refusal.Redirect})
	case errors.Is(err, oauth.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.fail(w, r, err)
	}
}

// decodeAuthorization reads the page's call: the authorization request's query and, for an approval, the caps.
func decodeAuthorization(w http.ResponseWriter, r *http.Request) (oauth.Request, oauth.Caps, bool) {
	var body struct {
		Query string `json:"query"`
		oauth.Caps
	}
	if !decode(w, r, &body) {
		return oauth.Request{}, oauth.Caps{}, false
	}
	query, err := url.ParseQuery(body.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the query is not a URL query")
		return oauth.Request{}, oauth.Caps{}, false
	}
	return oauth.ParseRequest(query), body.Caps, true
}
