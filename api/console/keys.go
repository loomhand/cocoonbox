package console

import (
	"context"
	"net/http"

	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/vault"
)

const keysPage = "/cb/keys"

type key struct {
	Name        string   `json:"name"`
	Hosts       []string `json:"hosts"`
	Header      string   `json:"header"`
	Template    string   `json:"template"`
	Query       string   `json:"query"`
	Placeholder string   `json:"placeholder"`
	Hint        string   `json:"hint"`
	CreatedAt   int64    `json:"created_at_ms"`
	UpdatedAt   int64    `json:"updated_at_ms"`
}

// EnableKeys serves the Keys page and its calls over keys.
func (s *Server) EnableKeys(keys controlv1.KeysServer) {
	s.keys = keys
	s.mux.HandleFunc("GET "+keysPage, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/keys.html")
	})
	s.mux.HandleFunc("GET "+Prefix+"api/keys", s.signedIn(s.listKeys))
	s.mux.HandleFunc("POST "+Prefix+"api/keys", s.signedIn(s.addKey))
	s.mux.HandleFunc("PUT "+Prefix+"api/keys/{name}", s.signedIn(s.rotateKey))
	s.mux.HandleFunc("DELETE "+Prefix+"api/keys/{name}", s.signedIn(s.deleteKey))
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	list, err := s.keys.List(s.keysCtx(r), &controlv1.ListKeysRequest{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]key, 0, len(list.GetKeys()))
	for _, k := range list.GetKeys() {
		out = append(out, toKey(k))
	}
	writeJSON(w, out)
}

func (s *Server) addKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string   `json:"name"`
		Hosts    []string `json:"hosts"`
		Header   string   `json:"header"`
		Template string   `json:"template"`
		Query    string   `json:"query"`
		Value    string   `json:"value"`
	}
	if !decode(w, r, &req) {
		return
	}
	k, err := s.keys.Add(s.keysCtx(r), &controlv1.AddKeyRequest{
		Name: req.Name, Hosts: req.Hosts, Header: req.Header, Template: req.Template, Query: req.Query, Value: req.Value,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toKey(k))
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Value string `json:"value"`
	}
	if !decode(w, r, &req) {
		return
	}
	k, err := s.keys.Rotate(s.keysCtx(r), &controlv1.RotateKeyRequest{Name: r.PathValue("name"), Value: req.Value})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toKey(k))
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if _, err := s.keys.Delete(s.keysCtx(r), &controlv1.KeyRef{Name: r.PathValue("name")}); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) keysCtx(r *http.Request) context.Context {
	return vault.WithActor(s.ownerCtx(r), vault.ActorConsole)
}

func toKey(k *controlv1.Key) key {
	return key{
		Name: k.GetName(), Hosts: k.GetHosts(), Header: k.GetHeader(), Template: k.GetTemplate(), Query: k.GetQuery(),
		Placeholder: k.GetPlaceholder(), Hint: k.GetHint(), CreatedAt: k.GetCreatedAtMs(), UpdatedAt: k.GetUpdatedAtMs(),
	}
}
