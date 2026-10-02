// Package console serves the cocoonbox web console under /cb/: password sign-in, the user's own page and the admin page.
package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/CMGS/cocoonbox/api/auth"
	controlv1 "github.com/CMGS/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/CMGS/cocoonbox/api/gwkeys"
	"github.com/CMGS/cocoonbox/api/users"
)

const (
	// Prefix is the path the console is served under.
	Prefix = "/cb/"

	cookieName  = "cb_session"
	cookieAge   = 12 * time.Hour
	failWindow  = 15 * time.Minute
	nameLimit   = 5
	addrLimit   = 20
	maxBodySize = 16 << 10
)

var (
	errTooMany = errors.New("too many failed sign-ins; try again later")

	//go:embed static
	static embed.FS
)

// Boxes lists a user's boxes and releases them when the user is deleted.
type Boxes interface {
	List(ctx context.Context, req *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error)
	Release(ctx context.Context, user string) error
}

type userKey struct{}

type box struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Image string `json:"image"`
}

// Server is the console's HTTP handler.
type Server struct {
	users   *users.Service
	boxes   Boxes
	limiter *limiter
	mux     *http.ServeMux

	defaultDailyCap int64
}

// New serves the console for users and their boxes; defaultDailyCap is gw's per-user default daily cap in micros.
func New(us *users.Service, boxes Boxes, defaultDailyCap int64) *Server {
	s := &Server{users: us, boxes: boxes, limiter: newLimiter(failWindow), mux: http.NewServeMux(), defaultDailyCap: defaultDailyCap}
	files, _ := fs.Sub(static, "static")
	s.mux.Handle("GET "+Prefix, http.StripPrefix(Prefix, http.FileServerFS(files)))
	s.mux.HandleFunc("POST "+Prefix+"api/login", s.login)
	s.mux.HandleFunc("POST "+Prefix+"api/logout", s.logout)
	s.mux.HandleFunc("GET "+Prefix+"api/me", s.signedIn(s.me))
	s.mux.HandleFunc("PATCH "+Prefix+"api/me", s.signedIn(s.updateMe))
	s.mux.HandleFunc("POST "+Prefix+"api/me/token", s.signedIn(s.regenerateToken))
	s.mux.HandleFunc("POST "+Prefix+"api/me/password", s.signedIn(s.changePassword))
	s.mux.HandleFunc("GET "+Prefix+"api/users", s.admin(s.listUsers))
	s.mux.HandleFunc("POST "+Prefix+"api/users", s.admin(s.createUser))
	s.mux.HandleFunc("PATCH "+Prefix+"api/users/{id}", s.admin(s.updateUser))
	s.mux.HandleFunc("POST "+Prefix+"api/users/{id}/password", s.admin(s.resetPassword))
	s.mux.HandleFunc("PUT "+Prefix+"api/users/{id}/daily-cap", s.admin(s.setDailyCap))
	s.mux.HandleFunc("DELETE "+Prefix+"api/users/{id}", s.admin(s.deleteUser))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// A JSON body is not a simple request, so a cross-site form or fetch cannot send one without a preflight.
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "requests must send application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	u, err := s.checkPassword(r, req.Name, req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	secret, err := s.users.NewSession(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: secret, Path: Prefix, MaxAge: int(cookieAge.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if err := s.users.EndSession(r.Context(), c.Value); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: Prefix, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := caller(r)
	resp := struct {
		User            users.User     `json:"user"`
		Boxes           []box          `json:"boxes"`
		Usage           []gwkeys.Usage `json:"usage"`
		UsageError      string         `json:"usage_error,omitempty"`
		DailyCap        *int64         `json:"daily_cap_micros"`
		DefaultDailyCap int64          `json:"default_daily_cap_micros,omitempty"`
		SpentToday      int64          `json:"spent_today_micros"`
	}{User: u, Boxes: []box{}, DefaultDailyCap: s.defaultDailyCap}
	list, err := s.boxes.List(auth.WithUser(r.Context(), u.ID), &controlv1.ListBoxesRequest{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, b := range list.GetBoxes() {
		resp.Boxes = append(resp.Boxes, box{ID: b.GetId(), State: strings.ToLower(strings.TrimPrefix(b.GetState().String(), "BOX_STATE_")), Image: b.GetImage()})
	}
	var today []gwkeys.Usage
	if resp.Usage, err = s.users.Usage(r.Context(), u.ID, time.Unix(0, 0)); err == nil {
		if today, err = s.users.Usage(r.Context(), u.ID, time.Now().UTC().Truncate(24*time.Hour)); err == nil {
			resp.DailyCap, err = s.users.DailyCap(r.Context(), u.ID)
		}
	}
	if err != nil {
		log.WithFunc("console.Server.me").WithField("user", u.ID).Error(r.Context(), err, "read gw usage and cap")
		resp.UsageError = "usage is unavailable right now"
	}
	for _, m := range today {
		resp.SpentToday += m.CostMicros
	}
	writeJSON(w, resp)
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Residential *bool `json:"residential"`
		MaxBoxes    *int  `json:"max_boxes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Residential != nil {
		writeError(w, http.StatusForbidden, "only an admin can change the residential exit")
		return
	}
	u, err := s.users.Update(r.Context(), caller(r).ID, users.Settings{MaxBoxes: req.MaxBoxes})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, u)
}

func (s *Server) regenerateToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.users.RegenerateToken(r.Context(), caller(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"token": token})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	u := caller(r)
	if _, err := s.checkPassword(r, u.Name, req.Current); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.users.SetPassword(r.Context(), u.ID, req.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	caps, err := s.users.DailyCaps(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type row struct {
		users.User
		DailyCap *int64 `json:"daily_cap_micros"`
	}
	list := s.users.List()
	rows := make([]row, 0, len(list))
	for _, u := range list {
		rw := row{User: u}
		if micros, ok := caps[u.ID]; ok {
			rw.DailyCap = &micros
		}
		rows = append(rows, rw)
	}
	writeJSON(w, rows)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "an initial password is required")
		return
	}
	u, err := s.users.Create(r.Context(), users.NewUser{Name: req.Name, Password: req.Password})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.createUser").Infof(r.Context(), "admin %s created user %s (%s)", caller(r).Name, u.Name, u.ID)
	writeJSON(w, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var set users.Settings
	if !decode(w, r, &set) {
		return
	}
	id := r.PathValue("id")
	if id == caller(r).ID && set.Disabled != nil && *set.Disabled {
		writeError(w, http.StatusBadRequest, "you cannot disable yourself")
		return
	}
	u, err := s.users.Update(r.Context(), id, set)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, u)
}

func (s *Server) setDailyCap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Micros *int64 `json:"micros"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.users.SetDailyCap(r.Context(), id, req.Micros); err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.setDailyCap").Infof(r.Context(), "admin %s set the gw daily cap of %s to %s", caller(r).Name, id, capText(req.Micros))
	writeJSON(w, map[string]*int64{"daily_cap_micros": req.Micros})
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.users.SetPassword(r.Context(), r.PathValue("id"), req.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == caller(r).ID {
		writeError(w, http.StatusBadRequest, "you cannot delete yourself")
		return
	}
	disabled := true
	if _, err := s.users.Update(r.Context(), id, users.Settings{Disabled: &disabled}); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.boxes.Release(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.users.Delete(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.deleteUser").Infof(r.Context(), "admin %s deleted user %s", caller(r).Name, id)
	w.WriteHeader(http.StatusNoContent)
}

// checkPassword checks name's password under the attempt limits.
func (s *Server) checkPassword(r *http.Request, name, password string) (users.User, error) {
	nameKey, addrKey := "name:"+name, "addr:"+clientAddr(r)
	if !s.limiter.take(nameKey, addrKey) {
		return users.User{}, errTooMany
	}
	u, err := s.users.Login(r.Context(), name, password)
	if err == nil {
		s.limiter.settle(nameKey, addrKey)
	} else if errors.Is(err, users.ErrBadPassword) {
		log.WithFunc("console.Server.checkPassword").Warnf(r.Context(), "wrong password for %q from %s", name, clientAddr(r))
	}
	return u, err
}

// signedIn runs next with the session's user in the request context.
func (s *Server) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		u, err := s.users.Session(r.Context(), c.Value)
		if errors.Is(err, users.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	}
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if !caller(r).Admin {
			writeError(w, http.StatusForbidden, "admins only")
			return
		}
		next(w, r)
	})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errTooMany):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, users.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, users.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, users.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, users.ErrBadPassword):
		writeError(w, http.StatusUnauthorized, err.Error())
	default:
		log.WithFunc("console.Server.fail").Errorf(r.Context(), err, "%s %s", r.Method, r.URL.Path)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func caller(r *http.Request) users.User {
	u, _ := r.Context().Value(userKey{}).(users.User)
	return u
}

// clientAddr is the address Caddy appended last to X-Forwarded-For; the api listens only on loopback behind it.
func clientAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func capText(micros *int64) string {
	switch {
	case micros == nil:
		return "the default"
	case *micros == gwkeys.Unlimited:
		return "unlimited"
	default:
		return strconv.FormatInt(*micros, 10) + " micros"
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
