package connecttest

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

const (
	pkceS256      = "S256"
	fieldCode     = "code"
	grantRefresh  = "refresh_token"
	errorKey      = "error"
	invalidGrant  = "invalid_grant"
	invalidClient = "invalid_client"
)

// Link is a mock of Link's authorization server and API: the publishable key as a Bearer token on every token and
// revoke request, the client secret in the form, PKCE, single-use rotating refresh tokens, revocation, and spend requests.
type Link struct {
	*httptest.Server
	ClientID     string
	ClientSecret string
	Key          string

	mu        sync.Mutex
	codes     map[string]string
	access    map[string]bool
	refresh   map[string]bool
	spends    []map[string]any
	failSpend bool
	refreshes int
	polls     int
	revoked   []string
}

// NewLink starts a mock Link with a client and a publishable key.
func NewLink() *Link {
	l := &Link{ClientID: "lwlc_test", ClientSecret: "lwls_test", Key: "pk_test_mock", codes: map[string]string{}, access: map[string]bool{}, refresh: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", l.authorize)
	mux.HandleFunc("POST /auth/token", l.token)
	mux.HandleFunc("POST /auth/revoke", l.revoke)
	mux.HandleFunc("GET /userinfo", l.userinfo)
	mux.HandleFunc("GET /spend_requests", l.spendRequests)
	l.Server = httptest.NewServer(mux)
	return l
}

// Consent answers an authorization URL the way a user's consent does, and returns the code and state it redirects with.
func (l *Link) Consent(authorizeURL string) (string, string, error) {
	return (&Vendor{}).Consent(authorizeURL)
}

// SetSpends sets what GET /spend_requests lists.
func (l *Link) SetSpends(spends ...map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spends = spends
}

// FailSpends makes GET /spend_requests answer 500.
func (l *Link) FailSpends(fail bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failSpend = fail
}

// ExpireAccess makes every access token invalid, as an expiry at Link does.
func (l *Link) ExpireAccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.access)
}

// Snapshot returns the counters under the lock.
func (l *Link) Snapshot() (refreshes, polls int, revoked []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refreshes, l.polls, append([]string(nil), l.revoked...)
}

func (l *Link) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("key") != l.Key || q.Get("client_id") != l.ClientID || q.Get("response_type") != fieldCode || q.Get("code_challenge_method") != pkceS256 ||
		!strings.Contains(" "+q.Get("scope")+" ", " payment_methods.agentic ") {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := token()
	l.mu.Lock()
	l.codes[code] = q.Get("code_challenge")
	l.mu.Unlock()
	http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{fieldCode: {code}, "state": {q.Get("state")}}.Encode(), http.StatusFound) //nolint:gosec // a mock authorization server redirects where its test asks
}

func (l *Link) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	if !l.client(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{errorKey: invalidClient})
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch f.Get("grant_type") {
	case "authorization_code":
		challenge, ok := l.codes[f.Get(fieldCode)]
		delete(l.codes, f.Get(fieldCode))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			writeJSON(w, http.StatusBadRequest, map[string]string{errorKey: invalidGrant})
			return
		}
	case grantRefresh:
		l.refreshes++
		if !l.refresh[f.Get(grantRefresh)] {
			writeJSON(w, http.StatusBadRequest, map[string]string{errorKey: invalidGrant})
			return
		}
		delete(l.refresh, f.Get(grantRefresh))
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{errorKey: "unsupported_grant_type"})
		return
	}
	access, refresh := "liwltoken_"+token(), "liwlrefresh_"+token()
	l.access[access], l.refresh[refresh] = true, true
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, grantRefresh: refresh, "token_type": "Bearer", "expires_in": accessTTL, "scope": "payment_methods.agentic userinfo:read",
	})
}

func (l *Link) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if !l.client(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{errorKey: invalidClient})
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := r.PostForm.Get("token")
	l.revoked = append(l.revoked, t)
	if l.refresh[t] {
		delete(l.refresh, t)
		clear(l.access)
	}
	w.WriteHeader(http.StatusOK)
}

func (l *Link) userinfo(w http.ResponseWriter, r *http.Request) {
	if !l.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"email": "buyer@example.com"})
}

func (l *Link) spendRequests(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.polls++
	switch {
	case !l.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]:
		w.WriteHeader(http.StatusUnauthorized)
	case l.failSpend:
		w.WriteHeader(http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"data": l.spends})
	}
}

func (l *Link) client(r *http.Request) bool {
	f := r.PostForm
	return r.Header.Get("Authorization") == "Bearer "+l.Key && f.Get("client_id") == l.ClientID && f.Get("client_secret") == l.ClientSecret
}

func (l *Link) authorized(r *http.Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
}
