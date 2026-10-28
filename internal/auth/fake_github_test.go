package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is a GitHub OAuth App provider and /user API for tests. It
// checks the client, the redirect URI and PKCE S256 like GitHub does.
type fakeGitHub struct {
	srv *httptest.Server

	clientID, clientSecret string

	mu     sync.Mutex
	next   githubAccount            // the user who approves the next authorize request
	codes  map[string]*fakeCode     // issued codes, each usable once
	tokens map[string]githubAccount // issued access tokens
	n      int

	// lastAuthorize is the query of the last authorize request
	lastAuthorize url.Values
	// userCalls counts the GET /user calls
	userCalls int
}

type fakeCode struct {
	challenge   string
	redirectURI string
	user        githubAccount
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		clientID:     "test-client",
		clientSecret: "test-secret",
		codes:        make(map[string]*fakeCode),
		tokens:       make(map[string]githubAccount),
		next:         githubAccount{ID: 1001, Login: "octocat", CreatedAt: time.Now().Add(-365 * 24 * time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", f.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", f.accessToken)
	mux.HandleFunc("GET /api/user", f.user)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) endpoints() Endpoints {
	e, err := GitHubEndpoints(f.srv.URL, f.srv.URL+"/api")
	if err != nil {
		panic(err)
	}
	return e
}

// setUser sets who approves the next login
func (f *fakeGitHub) setUser(u githubAccount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = u
}

// authorizeQuery returns the query of the last authorize request
func (f *fakeGitHub) authorizeQuery() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuthorize
}

// users returns how many GET /user calls were made
func (f *fakeGitHub) users() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userCalls
}

func (f *fakeGitHub) id() string {
	f.n++
	sum := sha256.Sum256([]byte{byte(f.n), byte(f.n >> 8)})
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize approves at once and sends the browser back with a code
func (f *fakeGitHub) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuthorize = q
	if q.Get("client_id") != f.clientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	code := f.id()
	f.codes[code] = &fakeCode{challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), user: f.next}
	back, _ := url.Parse(q.Get("redirect_uri"))
	back.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// accessToken trades a code for a token, checking the PKCE verifier
func (f *fakeGitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := func(code string) {
		// GitHub answers 200 with an error field
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if r.PostForm.Get("client_id") != f.clientID || r.PostForm.Get("client_secret") != f.clientSecret {
		fail("incorrect_client_credentials")
		return
	}
	c, ok := f.codes[r.PostForm.Get("code")]
	if !ok {
		fail("bad_verification_code")
		return
	}
	delete(f.codes, r.PostForm.Get("code"))
	if r.PostForm.Get("redirect_uri") != c.redirectURI {
		fail("redirect_uri_mismatch")
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if verifier == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
		fail("bad_verification_code")
		return
	}
	token := "gho_" + f.id()
	f.tokens[token] = c.user
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"access_token": token, "token_type": "bearer", "scope": ""})
}

// user returns the user of a token
func (f *fakeGitHub) user(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls++
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || auth[:7] != "Bearer " {
		http.Error(w, `{"message":"Requires authentication"}`, http.StatusUnauthorized)
		return
	}
	u, ok := f.tokens[auth[7:]]
	if !ok {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": u.ID, "login": u.Login, "created_at": u.CreatedAt.UTC().Format(time.RFC3339), "email": "hidden@example.com",
	})
}
