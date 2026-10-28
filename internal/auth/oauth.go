package auth

import (
	"container/list"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
)

const (
	// maxStates logins may be in progress at once; past that the oldest is dropped
	maxStates = 10000

	// stateTTL is how long a login may take at GitHub
	stateTTL = 10 * time.Minute

	// maxReturnTo is the longest return path kept
	maxReturnTo = 512

	// DefaultReturn is where a login lands without a usable return path
	DefaultReturn = "/dashboard"

	// maxUserBody caps the GitHub /user answer
	maxUserBody = 1 << 20
)

// githubLogin is the form of a GitHub user name
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// SafeReturn returns path when it is a local path on this server, else
// DefaultReturn: it must start with one /, not // or /\, hold no backslash,
// no control or space characters and no scheme, and be at most 512 bytes.
func SafeReturn(path string) string {
	if path == "" || len(path) > maxReturnTo || path[0] != '/' {
		return DefaultReturn
	}
	if strings.HasPrefix(path, "//") || strings.ContainsRune(path, '\\') {
		return DefaultReturn
	}
	for _, r := range path {
		// Browsers drop tabs and newlines in URLs, so "/\t/evil" would become "//evil"
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == unicode.ReplacementChar {
			return DefaultReturn
		}
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return DefaultReturn
	}
	return path
}

// loginState is a login in progress, kept on the server only
type loginState struct {
	state    string
	binding  []byte // the value of the oauth cookie of the browser that started it
	verifier string // the PKCE verifier
	returnTo string
	reauth   bool
	created  time.Time
}

// stateStore holds the logins in progress: bounded, each one used once and
// only within its lifetime
type stateStore struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	entries map[string]*list.Element
	order   *list.List // oldest first
	now     func() time.Time
}

func newStateStore(max int, ttl time.Duration) *stateStore {
	return &stateStore{max: max, ttl: ttl, entries: make(map[string]*list.Element), order: list.New(), now: time.Now}
}

// put adds a login, dropping expired ones and, when full, the oldest
func (s *stateStore) put(st *loginState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for front := s.order.Front(); front != nil; front = s.order.Front() {
		old := front.Value.(*loginState)
		if now.Sub(old.created) < s.ttl && s.order.Len() < s.max {
			break
		}
		s.order.Remove(front)
		delete(s.entries, old.state)
	}
	st.created = now
	s.entries[st.state] = s.order.PushBack(st)
}

// take removes a login and returns it, when it exists and has not expired
func (s *stateStore) take(state string) (*loginState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.entries[state]
	if !ok {
		return nil, false
	}
	s.order.Remove(el)
	delete(s.entries, state)
	st := el.Value.(*loginState)
	if s.now().Sub(st.created) >= s.ttl {
		return nil, false
	}
	return st, true
}

// len returns how many logins are kept
func (s *stateStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// handleLogin starts a GitHub login: it keeps a new state with its PKCE
// verifier and return path, binds it to this browser with the oauth cookie,
// and sends the browser to GitHub
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	st := &loginState{
		state:    encode(newSecret()),
		binding:  newSecret(),
		verifier: oauth2.GenerateVerifier(),
		returnTo: SafeReturn(query.Get("return")),
		reauth:   query.Get("reauth") == "1",
	}
	s.states.put(st)

	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.verifier)}
	if st.reauth {
		// Make GitHub ask which account to use instead of passing straight through
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "select_account"))
	}
	http.SetCookie(w, s.cookie(oauthCookie, encode(st.binding), int(stateTTL/time.Second)))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.oauth.AuthCodeURL(st.state, opts...), http.StatusFound)
}

// loginError answers a failed login with a short plain text page
func loginError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "%s\n\nGo back to the start page and try again: /\n", msg)
}

// handleCallback finishes a login: the state must be one this server gave
// out, unused, unexpired and started in this browser. The code is traded
// with the PKCE verifier, the GitHub token is used once to read the user and
// dropped, and a new session replaces any the browser had.
func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	binding, bound := decodeSecret(s.readCookie(r, oauthCookie))
	http.SetCookie(w, s.cookie(oauthCookie, "", -1))

	st, ok := s.states.take(query.Get("state"))
	if !ok {
		log.Info().Msg("Login refused: unknown, used or expired state")
		loginError(w, http.StatusBadRequest, "This login link has expired or was already used.")
		return
	}
	if !bound || subtle.ConstantTimeCompare(binding, st.binding) != 1 {
		log.Warn().Msg("Login refused: the state was started in another browser")
		loginError(w, http.StatusBadRequest, "This login was started in another browser.")
		return
	}
	if query.Get("error") != "" {
		loginError(w, http.StatusBadRequest, "GitHub login was cancelled.")
		return
	}
	code := query.Get("code")
	if code == "" {
		loginError(w, http.StatusBadRequest, "GitHub sent no login code.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*outboundTimeout)
	defer cancel()
	gh, err := s.githubUser(ctx, code, st.verifier)
	if err != nil {
		log.Warn().Err(err).Msg("Login failed: GitHub did not confirm the user")
		loginError(w, http.StatusBadGateway, "GitHub did not confirm the login.")
		return
	}

	user, err := s.loginUser(ctx, gh)
	if err != nil {
		var refused *refusedError
		if errors.As(err, &refused) {
			log.Info().Int64("github_id", gh.ID).Str("reason", refused.reason).Msg("Login refused")
			loginError(w, http.StatusForbidden, refused.message)
			return
		}
		log.Error().Err(err).Msg("Login failed: accounts database error")
		loginError(w, http.StatusServiceUnavailable, "The server could not log you in right now.")
		return
	}

	// Session fixation: whatever session the browser came with is dropped
	if old, ok := decodeSecret(s.readCookie(r, sessionCookie)); ok {
		if err := s.db.DeleteSession(ctx, hashSecret(old)); err != nil {
			log.Warn().Err(err).Msg("Failed to delete the previous session at login")
		}
	}
	id, csrf := newSecret(), newSecret()
	if err := s.db.CreateSession(ctx, hashSecret(id), user.ID, csrf, s.now().Add(s.cfg.SessionTTL)); err != nil {
		log.Error().Err(err).Msg("Login failed: could not store the session")
		loginError(w, http.StatusServiceUnavailable, "The server could not log you in right now.")
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, encode(id), int(s.cfg.SessionTTL/time.Second)))
	log.Info().Int64("user_id", user.ID).Int64("github_id", user.GitHubID).Bool("admin", s.IsAdmin(user.GitHubID)).
		Bool("reauth", st.reauth).Msg("User logged in")

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, st.returnTo, http.StatusSeeOther)
}

// githubAccount is what login reads from GitHub
type githubAccount struct {
	ID        int64     `json:"id"`
	Login     string    `json:"login"`
	CreatedAt time.Time `json:"created_at"`
}

// githubUser trades the code for a token and reads the user with it. The
// token is not kept.
func (s *Service) githubUser(ctx context.Context, code, verifier string) (*githubAccount, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.client)
	exchange, cancel := context.WithTimeout(ctx, outboundTimeout)
	defer cancel()
	token, err := s.oauth.Exchange(exchange, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("trading the code: %w", err)
	}

	read, cancel := context.WithTimeout(ctx, outboundTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(read, http.MethodGet, s.cfg.Endpoints.APIBase+"/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "hastebin")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading the user: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading the user: GitHub answered %d", res.StatusCode)
	}
	var gh githubAccount
	if err := json.NewDecoder(io.LimitReader(res.Body, maxUserBody)).Decode(&gh); err != nil {
		return nil, fmt.Errorf("reading the user: %w", err)
	}
	if gh.ID <= 0 || !githubLogin.MatchString(gh.Login) {
		return nil, errors.New("reading the user: no valid id or login")
	}
	return &gh, nil
}

// refusedError is a login GitHub confirmed that this server does not accept
type refusedError struct {
	reason  string // for the log
	message string // for the user
}

func (e *refusedError) Error() string { return "login refused: " + e.reason }

// loginUser finds or registers the account of a GitHub user. A new account
// needs a GitHub account at least MinAccountAge old, unless it is an admin's.
// A disabled account is refused.
func (s *Service) loginUser(ctx context.Context, gh *githubAccount) (*accounts.User, error) {
	user, _, err := s.db.LoginUser(ctx, gh.ID, gh.Login, false)
	if errors.Is(err, accounts.ErrNotFound) {
		if minAge := s.cfg.MinAccountAge; minAge >= 0 && !s.IsAdmin(gh.ID) {
			if gh.CreatedAt.IsZero() || s.now().Sub(gh.CreatedAt) < minAge {
				return nil, &refusedError{
					reason:  "GitHub account too new",
					message: fmt.Sprintf("Your GitHub account must be at least %d days old to register here.", int(math.Ceil(minAge.Hours()/24))),
				}
			}
		}
		var created bool
		user, created, err = s.db.LoginUser(ctx, gh.ID, gh.Login, true)
		if err == nil && created {
			log.Info().Int64("user_id", user.ID).Int64("github_id", gh.ID).Msg("New user registered")
		}
	}
	if err != nil {
		return nil, err
	}
	if user.Disabled() {
		return nil, &refusedError{reason: "account disabled", message: "This account is disabled."}
	}
	return user, nil
}
