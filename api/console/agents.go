package console

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"

	"github.com/loomhand/cocoonbox/api/oauth"
)

const (
	agentsPage = "/cb/agents"
	setupPage  = "/cb/setup"
)

// Agents checks and answers an agent's authorization request, and lists, revokes and acknowledges a user's grants and sets the boxes each may use.
type Agents interface {
	Check(ctx context.Context, user string, req oauth.Request) (oauth.Consent, error)
	Approve(ctx context.Context, user string, req oauth.Request, caps oauth.Caps, chosen []string) (string, error)
	Deny(ctx context.Context, req oauth.Request) (string, error)
	Grants(ctx context.Context, user string) ([]oauth.Grant, error)
	Choose(ctx context.Context, user, id string, boxes []string) error
	Revoke(ctx context.Context, user, id string) error
	MarkSeen(ctx context.Context, user string) error
}

// choice is one of the user's own boxes, which the user may let an agent use.
type choice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EnableAgents serves the consent page of oauth.AuthorizePath, the Agents page, the Connect your agent page and their calls over agents.
func (s *Server) EnableAgents(agents Agents) {
	s.agents = agents
	s.mux.HandleFunc("GET "+oauth.AuthorizePath, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/authorize.html")
	})
	s.mux.HandleFunc("GET "+agentsPage, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/agents.html")
	})
	s.mux.HandleFunc("GET "+setupPage, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/setup.html")
	})
	s.mux.HandleFunc("POST "+Prefix+"api/oauth/check", s.signedIn(s.checkAuthorization))
	s.mux.HandleFunc("POST "+Prefix+"api/oauth/approve", s.signedIn(s.approveAuthorization))
	s.mux.HandleFunc("POST "+Prefix+"api/oauth/deny", s.signedIn(s.denyAuthorization))
	s.mux.HandleFunc("GET "+Prefix+"api/agents", s.signedIn(s.listAgents))
	s.mux.HandleFunc("POST "+Prefix+"api/agents/seen", s.signedIn(s.markAgentsSeen))
	s.mux.HandleFunc("PUT "+Prefix+"api/agents/{id}/boxes", s.signedIn(s.chooseAgentBoxes))
	s.mux.HandleFunc("DELETE "+Prefix+"api/agents/{id}", s.signedIn(s.revokeAgent))
}

func (s *Server) checkAuthorization(w http.ResponseWriter, r *http.Request) {
	req, _, _, ok := decodeAuthorization(w, r)
	if !ok {
		return
	}
	u := caller(r)
	consent, err := s.agents.Check(r.Context(), u.ID, req)
	if err != nil {
		s.failAuthorization(w, r, err)
		return
	}
	writeJSON(w, struct {
		oauth.Consent
		User  string   `json:"user"`
		Boxes []choice `json:"boxes"`
	}{Consent: consent, User: u.Name, Boxes: s.choices(u.ID)})
}

func (s *Server) approveAuthorization(w http.ResponseWriter, r *http.Request) {
	req, caps, chosen, ok := decodeAuthorization(w, r)
	if !ok {
		return
	}
	redirect, err := s.agents.Approve(r.Context(), caller(r).ID, req, caps, chosen)
	if err != nil {
		s.failAuthorization(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"redirect": redirect})
}

func (s *Server) denyAuthorization(w http.ResponseWriter, r *http.Request) {
	req, _, _, ok := decodeAuthorization(w, r)
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
	u := caller(r)
	grants, err := s.agents.Grants(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, struct {
		Grants []oauth.Grant `json:"grants"`
		Boxes  []choice      `json:"boxes"`
	}{append([]oauth.Grant{}, grants...), s.choices(u.ID)})
}

func (s *Server) chooseAgentBoxes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boxes []string `json:"boxes"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.agents.Choose(r.Context(), caller(r).ID, r.PathValue("id"), body.Boxes)
	switch {
	case errors.Is(err, oauth.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.fail(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
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

// choices lists user's own boxes, the ones an agent may be let use, oldest first.
func (s *Server) choices(user string) []choice {
	out := []choice{}
	for _, b := range s.users.Boxes(user) {
		if !b.Agent {
			out = append(out, choice{ID: b.ID, Name: b.Name})
		}
	}
	return out
}

// decodeAuthorization reads the page's call: the authorization request's query and, for an approval, the caps and the chosen boxes.
func decodeAuthorization(w http.ResponseWriter, r *http.Request) (oauth.Request, oauth.Caps, []string, bool) {
	var body struct {
		Query string   `json:"query"`
		Boxes []string `json:"boxes"`
		oauth.Caps
	}
	if !decode(w, r, &body) {
		return oauth.Request{}, oauth.Caps{}, nil, false
	}
	query, err := url.ParseQuery(body.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the query is not a URL query")
		return oauth.Request{}, oauth.Caps{}, nil, false
	}
	return oauth.ParseRequest(query), body.Caps, body.Boxes, true
}
