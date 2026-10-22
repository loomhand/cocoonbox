// Package console serves the cocoonbox web console under /cb/: password sign-in, the user's own page, the admin page
// and the view-only page of a share at /cb/s/.
package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/net/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/loomhand/cocoonbox/api/auth"
	"github.com/loomhand/cocoonbox/api/connect"
	controlv1 "github.com/loomhand/cocoonbox/api/gen/cocoonbox/control/v1"
	"github.com/loomhand/cocoonbox/api/gwkeys"
	"github.com/loomhand/cocoonbox/api/signup"
	"github.com/loomhand/cocoonbox/api/users"
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
	removeLimit = time.Hour

	sharePrefix  = Prefix + "s/"
	shareHello   = 10 * time.Second
	shareCSP     = "default-src 'self'; img-src 'self' data:; connect-src 'self' wss://%s; frame-ancestors 'none'"
	maxSecretLen = 256
)

var (
	errTooMany = errors.New("too many failed sign-ins; try again later")

	//go:embed static
	static embed.FS
)

// Boxes lists, creates, renames and deletes a user's boxes, hibernates them when the user is disabled, and releases them and deletes their saved state when the user is deleted.
type Boxes interface {
	List(ctx context.Context, req *controlv1.ListBoxesRequest) (*controlv1.ListBoxesResponse, error)
	Create(ctx context.Context, req *controlv1.CreateBoxRequest) (*controlv1.EnsureResponse, error)
	Rename(ctx context.Context, req *controlv1.RenameBoxRequest) (*controlv1.Box, error)
	Delete(ctx context.Context, ref *controlv1.BoxRef) (*controlv1.Operation, error)
	Release(ctx context.Context, user string) error
	HibernateAll(ctx context.Context, user string) error
	DeleteState(ctx context.Context, user string) error
}

// Screens bridges a share's viewer to its box's screen, view-only.
type Screens interface {
	Screen(ctx context.Context, g auth.Grant, viewer io.ReadWriter) error
}

type userKey struct{}

type box struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Image string `json:"image"`
}

type share struct {
	ID        string   `json:"id"`
	Box       string   `json:"box_id"`
	Users     []string `json:"users"`
	Write     bool     `json:"write"`
	Secret    string   `json:"secret,omitempty"`
	ExpiresAt int64    `json:"expires_at_ms"`
}

// Server is the console's HTTP handler.
type Server struct {
	users   *users.Service
	boxes   Boxes
	limiter *limiter
	mux     *http.ServeMux
	shares  controlv1.SharesServer
	authn   *auth.Authenticator
	screens Screens
	connect *connect.Service
	keys    controlv1.KeysServer
	account controlv1.AccountServer
	agents  Agents
	signup  *signup.Service

	defaultDailyCap   int64
	defaultMaxRunning int
}

// New serves the console for users and their boxes; defaultDailyCap is gw's per-user default daily cap in micros,
// defaultMaxRunning the api's running limit for users without their own.
func New(us *users.Service, boxes Boxes, defaultDailyCap int64, defaultMaxRunning int) *Server {
	s := &Server{
		users: us, boxes: boxes, limiter: newLimiter(failWindow), mux: http.NewServeMux(),
		defaultDailyCap: defaultDailyCap, defaultMaxRunning: defaultMaxRunning,
	}
	files, _ := fs.Sub(static, "static")
	s.mux.Handle("GET "+Prefix, http.StripPrefix(Prefix, http.FileServerFS(files)))
	s.mux.HandleFunc("POST "+Prefix+"api/login", s.login)
	s.mux.HandleFunc("POST "+Prefix+"api/logout", s.logout)
	s.mux.HandleFunc("GET "+Prefix+"api/me", s.signedIn(s.me))
	s.mux.HandleFunc("PATCH "+Prefix+"api/me", s.signedIn(s.updateMe))
	s.mux.HandleFunc("POST "+Prefix+"api/me/token", s.signedIn(s.regenerateToken))
	s.mux.HandleFunc("DELETE "+Prefix+"api/me/token", s.signedIn(s.revokeToken))
	s.mux.HandleFunc("POST "+Prefix+"api/me/password", s.signedIn(s.changePassword))
	s.mux.HandleFunc("GET "+Prefix+"api/devices", s.signedIn(s.listDevices))
	s.mux.HandleFunc("DELETE "+Prefix+"api/devices/{id}", s.signedIn(s.revokeDevice))
	s.mux.HandleFunc("POST "+Prefix+"api/boxes", s.signedIn(s.createBox))
	s.mux.HandleFunc("PATCH "+Prefix+"api/boxes/{id}", s.signedIn(s.renameBox))
	s.mux.HandleFunc("DELETE "+Prefix+"api/boxes/{id}", s.signedIn(s.deleteBox))
	s.mux.HandleFunc("GET "+Prefix+"api/users", s.admin(s.listUsers))
	s.mux.HandleFunc("POST "+Prefix+"api/users", s.admin(s.createUser))
	s.mux.HandleFunc("PATCH "+Prefix+"api/users/{id}", s.admin(s.updateUser))
	s.mux.HandleFunc("POST "+Prefix+"api/users/{id}/password", s.admin(s.resetPassword))
	s.mux.HandleFunc("PUT "+Prefix+"api/users/{id}/daily-cap", s.admin(s.setDailyCap))
	s.mux.HandleFunc("DELETE "+Prefix+"api/users/{id}", s.admin(s.deleteUser))
	return s
}

// EnableAccount serves signing out everywhere over account.
func (s *Server) EnableAccount(account controlv1.AccountServer) {
	s.account = account
	s.mux.HandleFunc("POST "+Prefix+"api/me/sign-out-everywhere", s.signedIn(s.signOutEverywhere))
}

// EnableSharing serves the owner's share calls over shares and the share page, which authn admits and screens bridges.
func (s *Server) EnableSharing(shares controlv1.SharesServer, authn *auth.Authenticator, screens Screens) {
	s.shares, s.authn, s.screens = shares, authn, screens
	s.mux.HandleFunc("GET "+Prefix+"api/shares", s.signedIn(s.listShares))
	s.mux.HandleFunc("POST "+Prefix+"api/shares", s.signedIn(s.createShare))
	s.mux.HandleFunc("PATCH "+Prefix+"api/shares/{id}", s.signedIn(s.setShareWrite))
	s.mux.HandleFunc("POST "+Prefix+"api/shares/{id}/regenerate", s.signedIn(s.regenerateShare))
	s.mux.HandleFunc("DELETE "+Prefix+"api/shares/{id}", s.signedIn(s.revokeShare))
	s.mux.Handle("GET "+sharePrefix+"ws", websocket.Server{Handshake: sameOrigin, Handler: s.viewShare})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	csp := "default-src 'self'; frame-ancestors 'none'"
	if strings.HasPrefix(r.URL.Path, sharePrefix) {
		csp = fmt.Sprintf(shareCSP, r.Host)
	}
	w.Header().Set("Content-Security-Policy", csp)
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
	if err := s.startSession(w, r, u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
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
		DefaultRunning  int            `json:"default_max_running"`
		SpentToday      int64          `json:"spent_today_micros"`
		OwnBoxes        int            `json:"own_boxes"`
		Connect         bool           `json:"connect"`
	}{User: u, Boxes: []box{}, DefaultDailyCap: s.defaultDailyCap, DefaultRunning: s.defaultMaxRunning, Connect: s.connect != nil}
	for _, b := range s.users.Boxes(u.ID) {
		if !b.Agent {
			resp.OwnBoxes++
		}
	}
	list, err := s.boxes.List(s.ownerCtx(r), &controlv1.ListBoxesRequest{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, b := range list.GetBoxes() {
		resp.Boxes = append(resp.Boxes, toBox(b))
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
		MaxRunning  *int  `json:"max_running"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Residential != nil {
		writeError(w, http.StatusForbidden, "only an admin can change the residential exit")
		return
	}
	if req.MaxRunning != nil {
		writeError(w, http.StatusForbidden, "only an admin can change the running limit")
		return
	}
	u, err := s.users.Update(r.Context(), caller(r).ID, users.Settings{MaxBoxes: req.MaxBoxes})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, u)
}

func (s *Server) createBox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	created, err := s.boxes.Create(s.ownerCtx(r), &controlv1.CreateBoxRequest{Name: req.Name})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toBox(created.GetBox()))
}

func (s *Server) renameBox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	renamed, err := s.boxes.Rename(s.ownerCtx(r), &controlv1.RenameBoxRequest{BoxId: r.PathValue("id"), Name: req.Name})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toBox(renamed))
}

func (s *Server) deleteBox(w http.ResponseWriter, r *http.Request) {
	op, err := s.boxes.Delete(s.ownerCtx(r), &controlv1.BoxRef{BoxId: r.PathValue("id")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"operation_id": op.GetId()})
}

func (s *Server) regenerateToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.users.RegenerateToken(r.Context(), caller(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]string{"token": token})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.users.RevokeToken(r.Context(), caller(r).ID); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) signOutEverywhere(w http.ResponseWriter, r *http.Request) {
	if _, err := s.account.SignOutEverywhere(s.ownerCtx(r), &controlv1.SignOutEverywhereRequest{}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: Prefix, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.users.Devices(r.Context(), caller(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, append([]users.Device{}, devices...))
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.users.RevokeDevice(r.Context(), caller(r).ID, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	before, _ := s.users.Get(id)
	u, err := s.users.Update(r.Context(), id, set)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if u.Disabled && set.Disabled != nil {
		if err := s.boxes.HibernateAll(r.Context(), id); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if before.Admin && !u.Admin && u.IDP == signup.GitHub && s.signup != nil {
		if err := s.signupCap(r.Context(), id); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if u.Disabled || u.Admin != before.Admin {
		s.admit(r.Context())
	}
	writeJSON(w, u)
}

// signupCap gives a signup account the signup settings' daily cap.
func (s *Server) signupCap(ctx context.Context, id string) error {
	st, err := s.signup.Settings(ctx)
	if err != nil {
		return err
	}
	return s.users.SetDailyCap(ctx, id, new(st.DailyCapMicros))
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
	if err := s.removeUser(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	log.WithFunc("console.Server.deleteUser").Infof(r.Context(), "admin %s deleted user %s", caller(r).Name, id)
	s.admit(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// removeUser disables user id at once, then releases its boxes, deletes its saved state, revokes its connections and deletes it.
func (s *Server) removeUser(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeLimit)
	defer cancel()
	if _, err := s.users.Update(ctx, id, users.Settings{Disabled: new(true)}); err != nil {
		return err
	}
	if err := s.boxes.Release(ctx, id); err != nil {
		return err
	}
	if err := s.boxes.DeleteState(ctx, id); err != nil {
		return err
	}
	if s.connect != nil {
		if err := s.connect.RevokeAll(ctx, id); err != nil {
			return err
		}
	}
	return s.users.Delete(ctx, id)
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	list, err := s.shares.List(s.ownerCtx(r), &controlv1.ListSharesRequest{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]share, 0, len(list.GetShares()))
	for _, sh := range list.GetShares() {
		out = append(out, toShare(sh))
	}
	writeJSON(w, out)
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Box       string   `json:"box_id"`
		TTL       int64    `json:"ttl_seconds"`
		UserNames []string `json:"user_names"`
		Write     bool     `json:"write"`
	}
	if !decode(w, r, &req) {
		return
	}
	created, err := s.shares.Create(s.ownerCtx(r), &controlv1.CreateShareRequest{BoxId: req.Box, TtlSeconds: req.TTL, UserNames: req.UserNames, Write: req.Write})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"share": toShare(created.GetShare()), "secret": created.GetSecret()})
}

func (s *Server) setShareWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Write bool `json:"write"`
	}
	if !decode(w, r, &req) {
		return
	}
	sh, err := s.shares.SetWrite(s.ownerCtx(r), &controlv1.SetShareWriteRequest{ShareId: r.PathValue("id"), Write: req.Write})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, toShare(sh))
}

func (s *Server) regenerateShare(w http.ResponseWriter, r *http.Request) {
	regenerated, err := s.shares.Regenerate(s.ownerCtx(r), &controlv1.ShareRef{ShareId: r.PathValue("id")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"share": toShare(regenerated.GetShare()), "secret": regenerated.GetSecret()})
}

func (s *Server) revokeShare(w http.ResponseWriter, r *http.Request) {
	if _, err := s.shares.Revoke(s.ownerCtx(r), &controlv1.ShareRef{ShareId: r.PathValue("id")}); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// viewShare admits the secret in the first message, then bridges the screen of the share's box as binary messages.
// A named share takes the viewer from the console session cookie, so no token travels in the page.
func (s *Server) viewShare(ws *websocket.Conn) {
	r := ws.Request()
	ws.MaxPayloadBytes = maxSecretLen
	_ = ws.SetReadDeadline(time.Now().Add(shareHello))
	var secret string
	if websocket.Message.Receive(ws, &secret) != nil {
		return
	}
	_ = ws.SetReadDeadline(time.Time{})
	guest := ""
	if c, err := r.Cookie(cookieName); err == nil {
		if u, err := s.users.Session(r.Context(), c.Value); err == nil {
			guest = u.ID
		}
	}
	logger := log.WithFunc("console.Server.viewShare")
	grant, err := s.authn.Admit(secret, "", guest)
	if err != nil {
		logger.Infof(r.Context(), "share page refused from %s: %s", clientAddr(r), status.Code(err))
		_ = websocket.JSON.Send(ws, map[string]string{"error": status.Convert(err).Message(), codeKey: strings.ToLower(status.Code(err).String())})
		return
	}
	if websocket.JSON.Send(ws, map[string]bool{"ok": true}) != nil {
		return
	}
	ws.PayloadType = websocket.BinaryFrame
	ctx := r.Context()
	if guest != "" {
		var stop context.CancelFunc
		ctx, stop = s.authn.Bind(ctx, guest, "")
		defer stop()
	}
	start := time.Now()
	err = s.screens.Screen(ctx, grant, ws)
	logger.Infof(r.Context(), "share page of box %s as %s from %s: code=%s in %s",
		grant.Box, grant.Principal(), clientAddr(r), status.Code(err), time.Since(start).Round(time.Millisecond))
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user string) error {
	secret, err := s.users.NewSession(r.Context(), user)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: secret, Path: Prefix, MaxAge: int(cookieAge.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (s *Server) ownerCtx(r *http.Request) context.Context {
	return auth.WithUser(r.Context(), caller(r).ID)
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
	case errors.Is(err, signup.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, signup.ErrNotFound), errors.Is(err, signup.ErrOff):
		writeError(w, http.StatusNotFound, err.Error())
	case status.Code(err) == codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, status.Convert(err).Message())
	case status.Code(err) == codes.AlreadyExists:
		writeError(w, http.StatusConflict, status.Convert(err).Message())
	case status.Code(err) == codes.NotFound:
		writeError(w, http.StatusNotFound, status.Convert(err).Message())
	case status.Code(err) == codes.PermissionDenied:
		writeError(w, http.StatusForbidden, status.Convert(err).Message())
	case status.Code(err) == codes.FailedPrecondition, status.Code(err) == codes.ResourceExhausted:
		writeError(w, http.StatusConflict, status.Convert(err).Message())
	case status.Code(err) == codes.Unavailable:
		writeError(w, http.StatusBadGateway, status.Convert(err).Message())
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

// sameOrigin refuses a WebSocket that another site's page opens.
func sameOrigin(cfg *websocket.Config, r *http.Request) error {
	origin, err := websocket.Origin(cfg, r)
	if err != nil || origin == nil || origin.Host != r.Host {
		return errors.New("cross-origin share page")
	}
	return nil
}

func toBox(b *controlv1.Box) box {
	return box{ID: b.GetId(), Name: b.GetName(), State: strings.ToLower(strings.TrimPrefix(b.GetState().String(), "BOX_STATE_")), Image: b.GetImage()}
}

func toShare(sh *controlv1.Share) share {
	out := share{ID: sh.GetId(), Box: sh.GetBoxId(), Users: []string{}, Write: sh.GetWrite(), Secret: sh.GetSecret(), ExpiresAt: sh.GetExpiresAtMs()}
	for _, u := range sh.GetUsers() {
		out.Users = append(out.Users, u.GetName())
	}
	return out
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
