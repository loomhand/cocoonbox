package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/loomhand/cocoonbox/api/signup"
)

const (
	// loginCookie carries a sign-in's nonce to the callback, which the Strict, /cb/-scoped session cookie never reaches.
	loginCookie = "cb_login"
	loginAge    = 10 * time.Minute
	queueCookie = "cb_queue"
	queueAge    = 90 * 24 * time.Hour
	queuePage   = Prefix + "queue"
	invitesPath = Prefix + "api/invites"
	codeKey     = "code"
	bearer      = "bearer "
	loginLimit  = 20

	failedCode   = "failed"
	browserCode  = "browser"
	tooManyCode  = "too_many"
	idpErrorCode = "github_denied"
)

var errTooManyLogins = errors.New("too many sign-ins from this address; try again later")

// tokenHandler serves a call of the invite-scoped token whose id it gets.
type tokenHandler func(w http.ResponseWriter, r *http.Request, token string)

// EnableSignup serves sign-in with GitHub, linking, self-delete, the queue page and the admin's signup calls over sig, and the
// callback through LoginCallback.
func (s *Server) EnableSignup(sig *signup.Service) {
	s.signup = sig
	s.mux.HandleFunc("GET "+queuePage, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/queue.html")
	})
	s.mux.HandleFunc("GET "+Prefix+"api/auth/providers", s.providers)
	s.mux.HandleFunc("POST "+Prefix+"api/auth/github/begin", s.beginLogin)
	s.mux.HandleFunc("GET "+Prefix+"api/me/identities", s.signedIn(s.listIdentities))
	s.mux.HandleFunc("POST "+Prefix+"api/me/identities/github", s.signedIn(s.linkGitHub))
	s.mux.HandleFunc("DELETE "+Prefix+"api/me/identities/{idp}", s.signedIn(s.unlink))
	s.mux.HandleFunc("POST "+Prefix+"api/me/delete", s.signedIn(s.deleteMe))
	s.mux.HandleFunc("GET "+Prefix+"api/queue/me", s.myPlace)
	s.mux.HandleFunc("DELETE "+Prefix+"api/queue/me", s.leaveQueue)
	s.mux.HandleFunc("GET "+Prefix+"api/signup", s.admin(s.signupState))
	s.mux.HandleFunc("PUT "+Prefix+"api/signup", s.admin(s.setSignup))
	s.mux.HandleFunc("GET "+Prefix+"api/queue", s.admin(s.listQueue))
	s.mux.HandleFunc("POST "+Prefix+"api/queue/{id}/{action}", s.admin(s.actOnQueue))
	s.mux.HandleFunc("GET "+invitesPath, s.inviter(s.listInvites, s.listTokenInvites))
	s.mux.HandleFunc("POST "+invitesPath, s.inviter(s.createInvite, s.createTokenInvite))
	s.mux.HandleFunc("DELETE "+invitesPath+"/{id}", s.admin(s.revokeInvite))
	s.mux.HandleFunc("GET "+Prefix+"api/invite-tokens", s.admin(s.listInviteTokens))
	s.mux.HandleFunc("POST "+Prefix+"api/invite-tokens", s.admin(s.mintInviteToken))
	s.mux.HandleFunc("DELETE "+Prefix+"api/invite-tokens/{id}", s.admin(s.revokeInviteToken))
}

// LoginCallback finishes a sign-in with GitHub only in the browser that began it; a forged answer leaves the sign-in to its real one.
func (s *Server) LoginCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	logger := log.WithFunc("console.Server.LoginCallback")
	if !s.limiter.allow("login-callback:"+clientAddr(r), loginLimit) {
		loginRefused(w, r, tooManyCode)
		return
	}
	q := r.URL.Query()
	c, err := r.Cookie(loginCookie)
	if err != nil {
		logger.Warnf(r.Context(), "callback without the login cookie from %s", clientAddr(r))
		loginRefused(w, r, browserCode)
		return
	}
	if q.Get("error") != "" {
		loginRefused(w, r, idpErrorCode)
		return
	}
	out, err := s.signup.Complete(r.Context(), c.Value, q.Get("state"), q.Get("code"))
	if err != nil {
		logger.Warnf(r.Context(), "complete a sign-in with GitHub from %s: %v", clientAddr(r), err)
		loginRefused(w, r, failedCode)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: signup.CookiePath, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	switch out.Kind {
	case signup.SignedIn:
		if err := s.startSession(w, r, out.User); err != nil {
			logger.Error(r.Context(), err, "start a session")
			loginRefused(w, r, failedCode)
			return
		}
		http.Redirect(w, r, out.ReturnTo, http.StatusSeeOther)
	case signup.Linked:
		http.Redirect(w, r, Prefix+"?linked=github", http.StatusSeeOther)
	case signup.Queued:
		http.SetCookie(w, &http.Cookie{
			Name: queueCookie, Value: out.QueueSecret, Path: Prefix, MaxAge: int(queueAge.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, queuePage, http.StatusSeeOther)
	default:
		loginRefused(w, r, out.Reason)
	}
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	st, err := s.signup.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]bool{"github": s.signup.Configured(), "signup": s.signup.Configured() && st.Enabled, "invite_only": st.InviteOnly})
}

func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReturnTo string `json:"return_to"`
		Invite   string `json:"invite"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.begin(w, r, signup.BeginRequest{ReturnTo: req.ReturnTo, Invite: req.Invite})
}

func (s *Server) linkGitHub(w http.ResponseWriter, r *http.Request) {
	s.begin(w, r, signup.BeginRequest{Link: caller(r).ID})
}

func (s *Server) begin(w http.ResponseWriter, r *http.Request, req signup.BeginRequest) {
	if !s.limiter.allow("login-begin:"+clientAddr(r), loginLimit) {
		writeError(w, http.StatusTooManyRequests, errTooManyLogins.Error())
		return
	}
	started, err := s.signup.Begin(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: started.Nonce, Path: signup.CookiePath, MaxAge: int(loginAge.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]string{"url": started.URL})
}

func (s *Server) listIdentities(w http.ResponseWriter, r *http.Request) {
	ids, err := s.signup.Identities(r.Context(), caller(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]signup.Identity{}, ids...))
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	if idp := r.PathValue("idp"); idp == u.IDP {
		writeError(w, http.StatusConflict, "this account was made through "+idp+" and signs in only with it")
		return
	}
	if err := s.signup.Unlink(r.Context(), u.ID, r.PathValue("idp")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteMe deletes the caller's own account, which the caller confirms by its user name, and gives its seat to the queue.
func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	u := caller(r)
	switch {
	case u.Admin:
		writeError(w, http.StatusForbidden, "an admin's account is deleted by another admin")
		return
	case req.Name != u.Name:
		writeError(w, http.StatusBadRequest, "type your user name to confirm")
		return
	}
	if err := s.removeUser(r.Context(), u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.deleteMe").Infof(r.Context(), "user %s (%s) deleted its account", u.Name, u.ID)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: Prefix, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	s.admit(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) myPlace(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(queueCookie)
	if err != nil {
		writeError(w, http.StatusNotFound, "you are not in the queue")
		return
	}
	place, err := s.signup.Place(r.Context(), c.Value)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, place)
}

func (s *Server) leaveQueue(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(queueCookie)
	if err != nil {
		writeError(w, http.StatusNotFound, "you are not in the queue")
		return
	}
	if err := s.signup.Leave(r.Context(), c.Value); err != nil && !errors.Is(err, signup.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: queueCookie, Path: Prefix, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) signupState(w http.ResponseWriter, r *http.Request) {
	st, err := s.signup.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeSignup(w, r, st)
}

func (s *Server) setSignup(w http.ResponseWriter, r *http.Request) {
	var st signup.Settings
	if !decode(w, r, &st) {
		return
	}
	st, err := s.signup.SetSettings(r.Context(), st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.setSignup").Infof(r.Context(), "admin %s set signup enabled=%t invite_only=%t seats=%d", caller(r).Name, st.Enabled, st.InviteOnly, st.Seats)
	s.writeSignup(w, r, st)
}

func (s *Server) writeSignup(w http.ResponseWriter, r *http.Request, st signup.Settings) {
	seats, err := s.signup.Seats(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"settings": st, "seats": seats, "configured": s.signup.Configured()})
}

func (s *Server) listQueue(w http.ResponseWriter, r *http.Request) {
	queue, err := s.signup.Queue(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]signup.Entry{}, queue...))
}

func (s *Server) actOnQueue(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if err := s.signup.Act(r.Context(), id, action); err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.actOnQueue").Infof(r.Context(), "admin %s: %s queue entry %s", caller(r).Name, action, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := s.signup.Invites(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]signup.Invite{}, invites...))
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Uses int `json:"uses"`
		Days int `json:"days"`
	}
	if !decode(w, r, &req) {
		return
	}
	code, inv, err := s.signup.CreateInvite(r.Context(), caller(r).Name, req.Uses, req.Days)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"invite": inv, codeKey: code})
}

func (s *Server) listTokenInvites(w http.ResponseWriter, r *http.Request, token string) {
	invites, err := s.signup.TokenInvites(r.Context(), token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]signup.Invite{}, invites...))
}

// createTokenInvite makes a one-use invite for an invite-scoped token; uses, when sent, must be 1.
func (s *Server) createTokenInvite(w http.ResponseWriter, r *http.Request, token string) {
	var req struct {
		Uses int `json:"uses"`
		Days int `json:"days"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Uses != 0 && req.Uses != 1 {
		writeError(w, http.StatusBadRequest, "an invite made with a token has one use")
		return
	}
	code, inv, err := s.signup.CreateTokenInvite(r.Context(), token, req.Days)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.createTokenInvite").Infof(r.Context(), "invite token %s made invite %s", token, inv.ID)
	writeJSON(w, map[string]any{"invite": inv, codeKey: code})
}

func (s *Server) listInviteTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.signup.InviteTokens(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]signup.InviteToken{}, tokens...))
}

func (s *Server) mintInviteToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	if !decode(w, r, &req) {
		return
	}
	token, t, err := s.signup.MintInviteToken(r.Context(), caller(r).ID, req.Label)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.mintInviteToken").Infof(r.Context(), "admin %s minted invite token %s", caller(r).Name, t.ID)
	writeJSON(w, map[string]any{"invite_token": t, "token": token})
}

func (s *Server) revokeInviteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.signup.RevokeInviteToken(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.revokeInviteToken").Infof(r.Context(), "admin %s revoked invite token %s", caller(r).Name, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// inviter runs scoped for a request with an invite-scoped bearer token, which it resolves here, and admin for an admin session.
func (s *Server) inviter(admin http.HandlerFunc, scoped tokenHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := inviteBearer(r)
		if !ok {
			s.admin(admin)(w, r)
			return
		}
		id, err := s.signup.Inviter(r.Context(), token)
		if errors.Is(err, signup.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid invite token")
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		scoped(w, r, id)
	}
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	if err := s.signup.RevokeInvite(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// admit gives seats freed by a delete or a disable to the queue; signup's own timer retries a failure.
func (s *Server) admit(ctx context.Context) {
	if s.signup == nil {
		return
	}
	if err := s.signup.Admit(ctx); err != nil {
		log.WithFunc("console.Server.admit").Warnf(ctx, "admit after a freed seat: %v", err)
	}
}

// inviteBearer returns the invite-scoped token of the request's Authorization header.
func inviteBearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) <= len(bearer) || !strings.EqualFold(header[:len(bearer)], bearer) {
		return "", false
	}
	token := header[len(bearer):]
	return token, strings.HasPrefix(token, signup.InviteTokenPrefix)
}

// loginRefused sends the browser to the sign-in page with a reason code, which the page turns into text, so a link cannot put words on it.
func loginRefused(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, Prefix+"?"+url.Values{"login_error": {code}}.Encode(), http.StatusSeeOther)
}
