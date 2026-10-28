package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

// ErrDisabled is returned for a session of a disabled account
var ErrDisabled = errors.New("account disabled")

// Principal is who sends a request
type Principal struct {
	User *accounts.User

	// Session is the cookie session of the request, nil for a token
	Session *accounts.Session

	// Token is the API token of the request, nil for a session
	Token *accounts.Token

	// Admin is set when the user's GitHub id is in the admin list
	Admin bool
}

// CSRF returns the CSRF token of the session, in the form the X-CSRF-Token header carries
func (p *Principal) CSRF() string {
	if p == nil || p.Session == nil {
		return ""
	}
	return encode(p.Session.CSRF)
}

// ValidCSRF reports whether token, in the X-CSRF-Token form, is the CSRF
// token of the session, compared in constant time
func (p *Principal) ValidCSRF(token string) bool {
	if p == nil || p.Session == nil {
		return false
	}
	b, ok := decodeSecret(token)
	return ok && subtle.ConstantTimeCompare(b, p.Session.CSRF) == 1
}

type principalKey struct{}

// lookup is what LoadSession found
type lookup struct {
	principal *Principal
	err       error // ErrDisabled, or a database error
}

// FromContext returns the principal LoadSession found, nil for anonymous
func FromContext(ctx context.Context) *Principal {
	l, _ := ctx.Value(principalKey{}).(*lookup)
	if l == nil {
		return nil
	}
	return l.principal
}

// lookupFromContext returns what LoadSession found, nil when it did not run
func lookupFromContext(ctx context.Context) *lookup {
	l, _ := ctx.Value(principalKey{}).(*lookup)
	return l
}

// Session resolves the session cookie of a request. It returns nil without
// error when there is no valid session, ErrDisabled for a disabled account,
// and a database error otherwise. Sessions past their end, or idle too long,
// are deleted. The last use is written at most once a minute.
func (s *Service) Session(r *http.Request) (*Principal, error) {
	id, ok := decodeSecret(s.readCookie(r, sessionCookie))
	if !ok {
		return nil, nil
	}
	ctx := r.Context()
	hash := hashSecret(id)
	session, user, err := s.db.SessionByHash(ctx, hash)
	if errors.Is(err, accounts.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	now := s.now()
	if !now.Before(session.ExpiresAt) || now.Sub(session.LastSeenAt) >= s.cfg.SessionIdle {
		if err := s.db.DeleteSession(ctx, hash); err != nil {
			log.Warn().Err(err).Msg("Failed to delete an expired session")
		}
		return nil, nil
	}
	if user.Disabled() {
		return nil, ErrDisabled
	}
	if now.Sub(session.LastSeenAt) >= touchInterval {
		if err := s.db.TouchSession(ctx, hash, user.ID); err != nil {
			log.Warn().Err(err).Msg("Failed to record the use of a session")
		}
	}
	return &Principal{User: user, Session: session, Admin: s.IsAdmin(user.GitHubID)}, nil
}

// LoadSession resolves the session cookie and keeps the result in the
// request context, for FromContext and the Require middlewares
func (s *Service) LoadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Session(r)
		if err != nil && !errors.Is(err, ErrDisabled) {
			log.Error().Err(err).Msg("Failed to read a session")
		}
		ctx := context.WithValue(r.Context(), principalKey{}, &lookup{principal: p, err: err})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WriteError answers with the JSON error of the /api/v1 routes
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// writeLookupError answers a request whose session could not be used.
// It returns false when there was nothing wrong.
func writeLookupError(w http.ResponseWriter, l *lookup) bool {
	switch {
	case l == nil:
		WriteError(w, http.StatusInternalServerError, "internal", "Session middleware missing.")
	case errors.Is(l.err, ErrDisabled):
		WriteError(w, http.StatusForbidden, "disabled", "This account is disabled.")
	case accounts.IsBusy(l.err):
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "db_busy", "The server is busy, try again shortly.")
	case l.err != nil:
		WriteError(w, http.StatusInternalServerError, "internal", "The session could not be read.")
	default:
		return false
	}
	return true
}

// RequireSession lets through only requests with a valid session: 401
// without one, 403 for a disabled account. It needs LoadSession first.
func (s *Service) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := lookupFromContext(r.Context())
		if writeLookupError(w, l) {
			return
		}
		if l.principal == nil {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "Log in first.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireCSRF guards state changes made with the session cookie: the body
// must be JSON, X-CSRF-Token must hold the session's token, and Origin, when
// sent, must be the server's own. GET, HEAD and OPTIONS pass. It needs
// RequireSession first.
func (s *Service) RequireCSRF(next http.Handler) http.Handler {
	return s.requireCSRF(next, true)
}

// RequireCSRFRaw is RequireCSRF for routes whose body is the content of a
// paste, of any type: X-CSRF-Token and Origin are checked the same way.
// A form on another site cannot send the header, so it still cannot post.
func (s *Service) RequireCSRFRaw(next http.Handler) http.Handler {
	return s.requireCSRF(next, false)
}

func (s *Service) requireCSRF(next http.Handler, wantJSON bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if msg := s.checkCSRF(r, FromContext(r.Context()), wantJSON); msg != "" {
			// The route pattern, not the path, which may hold a paste key
			log.Info().Str("route", routePattern(r)).Str("reason", msg).Msg("Request refused by the CSRF check")
			WriteError(w, http.StatusForbidden, "csrf", msg)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// routePattern is the chi pattern of the route a request matched, such as
// /api/v1/pastes/{key}, "" outside chi
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		return rc.RoutePattern()
	}
	return ""
}

// checkCSRF returns why a state change is refused, "" when it may go on
func (s *Service) checkCSRF(r *http.Request, p *Principal, wantJSON bool) string {
	if p == nil || p.Session == nil {
		return "No session."
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return "Cross-site request."
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
		return "Wrong origin."
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); wantJSON && (err != nil || media != "application/json") {
		return "The body must be JSON."
	}
	if !p.ValidCSRF(r.Header.Get("X-CSRF-Token")) {
		return "Missing or wrong CSRF token."
	}
	return ""
}

// sameOrigin reports whether an Origin header names this server: the
// origin of BaseURL, or without one, the request's own host
func (s *Service) sameOrigin(r *http.Request, origin string) bool {
	origin = strings.ToLower(origin)
	if s.origin != "" {
		return origin == s.origin
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && strings.EqualFold(u.Host, r.Host)
}

// RequireAdmin lets through only admins with a session cookie. Everyone else,
// and any request with an Authorization header, gets 404 as if the route did
// not exist. It needs LoadSession first.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := FromContext(r.Context())
		if r.Header.Get("Authorization") != "" || p == nil || p.Session == nil || !s.IsAdmin(p.User.GitHubID) {
			NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRecentAuth lets through only sessions that went through GitHub
// within maxAge, else 403 reauth_required with the login link that renews
// it. Destructive admin actions use it. It needs RequireSession first.
func (s *Service) RequireRecentAuth(maxAge time.Duration, returnTo string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := FromContext(r.Context())
			if p == nil || p.Session == nil || s.now().Sub(p.Session.AuthAt) > maxAge {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{
					"error":   "reauth_required",
					"message": "Log in with GitHub again to do this.",
					"login":   "/auth/github/login?reauth=1&return=" + url.QueryEscape(SafeReturn(returnTo)),
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NotFound answers 404: JSON on /api/ routes, plain text elsewhere
func NotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		WriteError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	http.NotFound(w, r)
}

// RegisterRoutes adds the login routes, when login is on, and the session
// API. Without login /auth/* answers 404 and existing sessions keep working.
func (s *Service) RegisterRoutes(r chi.Router) {
	if s.login {
		r.With(s.loginLimit).Get("/auth/github/login", s.handleLogin)
		r.Get(CallbackPath, s.handleCallback)
	}
	r.HandleFunc("/auth/*", NotFound)

	r.Route("/api/v1/session", func(r chi.Router) {
		r.Use(s.LoadSession)
		r.Get("/", s.handleSession)
		r.With(s.RequireSession, s.RequireCSRF).Post("/logout", s.handleLogout)
		r.With(s.RequireSession, s.RequireCSRF).Post("/logout-all", s.handleLogoutAll)
	})
}

// handleSession answers GET /api/v1/session: the user (avatar_url null when
// there is none), the CSRF token for state changes and the limits, or user
// null without a session
func (s *Service) handleSession(w http.ResponseWriter, r *http.Request) {
	l := lookupFromContext(r.Context())
	if l != nil && l.err != nil && !errors.Is(l.err, ErrDisabled) {
		writeLookupError(w, l)
		return
	}
	p := FromContext(r.Context())
	body := map[string]any{"user": nil, "csrf": nil, "limits": nil}
	if p != nil {
		body["user"] = map[string]any{"id": p.User.ID, "login": p.User.Login, "admin": s.IsAdmin(p.User.GitHubID),
			"avatar_url": sizedAvatar(p.User.AvatarURL)}
		body["csrf"] = p.CSRF()
	}
	if s.cfg.Limits != nil {
		body["limits"] = s.cfg.Limits(p)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(body)
}

// handleLogout ends the session of the request
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	p := FromContext(r.Context())
	if err := s.db.DeleteSession(r.Context(), p.Session.IDHash); err != nil {
		log.Error().Err(err).Msg("Failed to delete a session at logout")
		WriteError(w, http.StatusServiceUnavailable, "db_busy", "Logging out failed, try again.")
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
	log.Info().Int64("user_id", p.User.ID).Msg("User logged out")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// maxLogoutBody caps the body of logout-all
const maxLogoutBody = 4 << 10

// handleLogoutAll ends every session of the user and, with revoke_tokens,
// revokes every API token, and records it in the audit log
func (s *Service) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RevokeTokens bool `json:"revoke_tokens"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxLogoutBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, "invalid_input", "The body must be {\"revoke_tokens\": true|false}.")
		return
	}

	p := FromContext(r.Context())
	sessions, tokens, err := s.db.LogoutAll(r.Context(), p.User, in.RevokeTokens)
	if err != nil {
		log.Error().Err(err).Msg("Failed to log out everywhere")
		WriteError(w, http.StatusServiceUnavailable, "db_busy", "Logging out failed, try again.")
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
	log.Info().Int64("user_id", p.User.ID).Int64("sessions", sessions).Int64("tokens", tokens).Msg("User logged out everywhere")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
