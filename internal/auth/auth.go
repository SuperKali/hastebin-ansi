// Package auth logs users in with GitHub (an OAuth App, state and PKCE) and
// keeps their server-side sessions: cookies, CSRF checks, admin checks.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/go-chi/httprate"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
)

const (
	// DefaultOAuthBase and DefaultAPIBase are GitHub's own addresses
	DefaultOAuthBase = "https://github.com"
	DefaultAPIBase   = "https://api.github.com"

	// CallbackPath is where GitHub sends the browser back, after BASE_URL
	CallbackPath = "/auth/github/callback"

	// outboundTimeout caps each call to GitHub
	outboundTimeout = 10 * time.Second

	// loginLimit login starts are allowed per client in loginWindow
	loginLimit  = 20
	loginWindow = 10 * time.Minute

	// touchInterval is the least time between two writes of a session's last use
	touchInterval = time.Minute

	// sweepInterval is how often sessions past their end are deleted
	sweepInterval = time.Hour

	// cookie names, after the __Host- prefix when cookies are Secure
	sessionCookie = "hb_session"
	oauthCookie   = "hb_oauth"

	// secretBytes is the size of session ids, CSRF tokens, OAuth states and bindings
	secretBytes = 32
)

// Endpoints are the GitHub addresses used for login
type Endpoints struct {
	AuthURL  string
	TokenURL string
	APIBase  string
}

// GitHubEndpoints returns the endpoints under oauthBase (https://github.com
// when empty) and apiBase (https://api.github.com when empty). Other bases
// are for a fake GitHub in tests and must be https, or http on localhost.
func GitHubEndpoints(oauthBase, apiBase string) (Endpoints, error) {
	oauthBase, err := checkBase("github_oauth_base", oauthBase, DefaultOAuthBase)
	if err != nil {
		return Endpoints{}, err
	}
	apiBase, err = checkBase("github_api_base", apiBase, DefaultAPIBase)
	if err != nil {
		return Endpoints{}, err
	}
	return Endpoints{
		AuthURL:  oauthBase + "/login/oauth/authorize",
		TokenURL: oauthBase + "/login/oauth/access_token",
		APIBase:  apiBase,
	}, nil
}

// checkBase validates an endpoint base, def when empty
func checkBase(name, base, def string) (string, error) {
	if base == "" {
		return def, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s %q is not a valid base URL", name, base)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return "", fmt.Errorf("%s %q must be https (http only on localhost or 127.0.0.1)", name, base)
	}
	return strings.TrimRight(base, "/"), nil
}

// isLoopbackHost reports whether host names this machine
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// CheckBaseURL validates the public URL that login needs: https, or http on
// localhost for local tests, with no path. secure is false only for the
// localhost exception, where cookies lose Secure and the __Host- prefix.
func CheckBaseURL(base string) (secure bool, err error) {
	if base == "" {
		return false, errors.New("base_url (BASE_URL) must be set when GitHub login is configured: cookies and the callback URL come from it")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false, fmt.Errorf("base_url %q must be the bare address of the server, for example https://paste.example.com", base)
	}
	switch {
	case u.Scheme == "https":
		return true, nil
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		return false, nil
	}
	return false, fmt.Errorf("base_url %q must be https when GitHub login is configured (http only on localhost)", base)
}

// Config is the login and session setup
type Config struct {
	// ClientID and ClientSecret are the GitHub OAuth App. Empty ClientID
	// turns login off: sessions made before still work, new logins 404.
	ClientID     string
	ClientSecret string

	// BaseURL is the public address. Cookie flags, the callback URL and the
	// Origin check come from it, never from the request.
	BaseURL string

	Endpoints Endpoints

	// SessionTTL is how long a session lasts at most, SessionIdle without use
	SessionTTL  time.Duration
	SessionIdle time.Duration

	// AdminIDs are the GitHub ids of the admins
	AdminIDs []int64

	// MinAccountAge is how old a GitHub account must be to register, < 0 for no check
	MinAccountAge time.Duration

	// HTTPClient makes the calls to GitHub. nil means one with a 10 second timeout.
	HTTPClient *http.Client

	// ClientKey names the client of a request for the login rate limit.
	// nil means the remote address, an IPv6 /64 counted as one.
	ClientKey func(*http.Request) string

	// Limits is sent in GET /api/v1/session, nil for none
	Limits func(*Principal) any
}

// Service logs users in and resolves their sessions
type Service struct {
	db  *accounts.DB
	cfg Config

	login  bool
	secure bool
	prefix string // "__Host-" when cookies are Secure
	origin string // scheme://host of BaseURL, "" when unknown

	oauth  *oauth2.Config
	client *http.Client
	states *stateStore
	admins map[int64]bool

	loginLimit func(http.Handler) http.Handler

	now func() time.Time
}

// New sets up login and sessions on db. With a ClientID it needs a secret
// and a BaseURL that CheckBaseURL accepts.
func New(db *accounts.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("no accounts database")
	}
	if cfg.SessionTTL <= 0 || cfg.SessionIdle <= 0 {
		return nil, errors.New("session lifetimes must be positive")
	}
	s := &Service{
		db:     db,
		cfg:    cfg,
		login:  cfg.ClientID != "",
		states: newStateStore(maxStates, stateTTL),
		admins: make(map[int64]bool, len(cfg.AdminIDs)),
		now:    time.Now,
	}
	for _, id := range cfg.AdminIDs {
		s.admins[id] = true
	}

	if s.login {
		if cfg.ClientSecret == "" {
			return nil, errors.New("github_client_secret (GITHUB_CLIENT_SECRET) must be set with github_client_id")
		}
		secure, err := CheckBaseURL(cfg.BaseURL)
		if err != nil {
			return nil, err
		}
		s.secure = secure
		if cfg.Endpoints.AuthURL == "" {
			if cfg.Endpoints, err = GitHubEndpoints("", ""); err != nil {
				return nil, err
			}
			s.cfg.Endpoints = cfg.Endpoints
		}
	} else if base, err := url.Parse(cfg.BaseURL); err == nil {
		// Without login cookies are only read and cleared; keep the strict
		// form unless the server is plainly local over http
		s.secure = !(base.Scheme == "http" && isLoopbackHost(base.Hostname()))
	}
	if s.secure {
		s.prefix = "__Host-"
	}
	if base, err := url.Parse(cfg.BaseURL); err == nil && base.Host != "" {
		s.origin = strings.ToLower(base.Scheme + "://" + base.Host)
	}

	s.client = cfg.HTTPClient
	if s.client == nil {
		s.client = &http.Client{Timeout: outboundTimeout}
	}
	s.oauth = &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   cfg.Endpoints.AuthURL,
			TokenURL:  cfg.Endpoints.TokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: strings.TrimRight(cfg.BaseURL, "/") + CallbackPath,
		// No scopes: the public profile (id, login, created_at) is all that is needed
	}

	clientKey := cfg.ClientKey
	if clientKey == nil {
		clientKey = remoteKey
	}
	s.loginLimit = httprate.LimitBy(loginLimit, loginWindow, func(r *http.Request) (string, error) {
		return clientKey(r), nil
	})
	return s, nil
}

// remoteKey is the remote address of a request, an IPv6 /64 counted as one
func remoteKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return httprate.CanonicalizeIP(host)
}

// LoginEnabled reports whether new logins are possible
func (s *Service) LoginEnabled() bool {
	return s.login
}

// IsAdmin reports whether a GitHub id is an admin
func (s *Service) IsAdmin(githubID int64) bool {
	return s.admins[githubID]
}

// SecureCookies reports whether cookies are Secure with the __Host- prefix
func (s *Service) SecureCookies() bool {
	return s.secure
}

// Run deletes sessions past their end every hour until ctx is done
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.db.DeleteStaleSessions(ctx, s.cfg.SessionIdle)
			if err != nil && ctx.Err() == nil {
				log.Warn().Err(err).Msg("Failed to delete expired sessions")
			} else if n > 0 {
				log.Info().Int64("deleted", n).Msg("Deleted expired sessions")
			}
		}
	}
}

// newSecret returns secretBytes random bytes
func newSecret() []byte {
	b := make([]byte, secretBytes)
	// crypto/rand.Read never fails on supported platforms; it panics instead
	_, _ = rand.Read(b)
	return b
}

// encode turns a secret into its cookie, header or URL form
func encode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeSecret parses the encoded form of a secret, false when it is not one
func decodeSecret(s string) ([]byte, bool) {
	if len(s) != base64.RawURLEncoding.EncodedLen(secretBytes) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != secretBytes {
		return nil, false
	}
	return b, true
}

// hashSecret is what the database stores for a session id
func hashSecret(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// cookie returns a cookie of this server: Path=/, HttpOnly, SameSite=Lax,
// Secure with the __Host- prefix unless the base URL is http on localhost.
// maxAge < 0 deletes it.
func (s *Service) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     s.prefix + name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// readCookie returns the value of one of this server's cookies, "" when missing
func (s *Service) readCookie(r *http.Request, name string) string {
	c, err := r.Cookie(s.prefix + name)
	if err != nil {
		return ""
	}
	return c.Value
}
