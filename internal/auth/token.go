package auth

import (
	"crypto/sha256"
	"errors"
	"hash/crc32"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/rs/zerolog/log"
)

const (
	// TokenPrefix starts every personal token, so scanners and people can tell one
	TokenPrefix = "hbp_"

	// tokenBodyLen is the base62 length of 32 random bytes, tokenSumLen of a CRC32
	tokenBodyLen = 43
	tokenSumLen  = 6

	// TokenLen is the length of a whole token
	TokenLen = len(TokenPrefix) + tokenBodyLen + tokenSumLen

	// tokenDisplayLen is how much of a token is stored and shown to recognise it: hbp_ab12cd
	tokenDisplayLen = len(TokenPrefix) + 6

	// tokenTouchInterval is the least time between two writes of a token's last use
	tokenTouchInterval = time.Minute
)

// ErrInvalidToken is returned for a bearer token that is malformed, unknown,
// revoked or expired
var ErrInvalidToken = errors.New("invalid token")

const base62Digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62 writes n in base62, padded with leading zeros to width
func base62(n *big.Int, width int) string {
	out := make([]byte, width)
	for i := range out {
		out[i] = '0'
	}
	v := new(big.Int).Set(n)
	base := big.NewInt(62)
	mod := new(big.Int)
	for i := width - 1; i >= 0 && v.Sign() > 0; i-- {
		v.DivMod(v, base, mod)
		out[i] = base62Digits[mod.Int64()]
	}
	return string(out)
}

// tokenChecksum is the base62 CRC32 of the part of a token before it
func tokenChecksum(head string) string {
	return base62(new(big.Int).SetUint64(uint64(crc32.ChecksumIEEE([]byte(head)))), tokenSumLen)
}

// NewToken returns a new personal token: hbp_, 32 random bytes in base62 and
// a 6 character base62 CRC32 of what comes before it
func NewToken() string {
	head := TokenPrefix + base62(new(big.Int).SetBytes(newSecret()), tokenBodyLen)
	return head + tokenChecksum(head)
}

// ValidTokenFormat reports whether s has the form of a personal token and a
// matching checksum. It needs no database, so typos and random guesses are
// refused before any lookup.
func ValidTokenFormat(s string) bool {
	if len(s) != TokenLen || !strings.HasPrefix(s, TokenPrefix) {
		return false
	}
	for i := len(TokenPrefix); i < len(s); i++ {
		if strings.IndexByte(base62Digits, s[i]) < 0 {
			return false
		}
	}
	split := len(s) - tokenSumLen
	return tokenChecksum(s[:split]) == s[split:]
}

// HashToken is what the database stores for a token
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// TokenDisplay is the start of a token that is stored and shown to recognise it
func TokenDisplay(token string) string {
	if len(token) < tokenDisplayLen {
		return token
	}
	return token[:tokenDisplayLen]
}

// bearerToken returns the token of an Authorization: Bearer header. present
// is false when the request has no such header; other schemes are ignored.
func bearerToken(r *http.Request) (token string, present bool) {
	header := r.Header.Get("Authorization")
	scheme, value, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// HasBearer reports whether a request carries an Authorization: Bearer header
func HasBearer(r *http.Request) bool {
	_, present := bearerToken(r)
	return present
}

// Bearer resolves the Authorization: Bearer header of a request. It returns
// nil without error when there is none. A token that is malformed, unknown,
// revoked or expired gives ErrInvalidToken, a disabled account ErrDisabled,
// and anything else is a database error. Tokens are never read from the
// query string. The last use is written at most once a minute.
func (s *Service) Bearer(r *http.Request) (*Principal, error) {
	token, present := bearerToken(r)
	if !present {
		return nil, nil
	}
	if !ValidTokenFormat(token) {
		return nil, ErrInvalidToken
	}
	ctx := r.Context()
	t, user, err := s.db.TokenByHash(ctx, HashToken(token))
	if errors.Is(err, accounts.ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt) {
		return nil, ErrInvalidToken
	}
	if user.Disabled() {
		return nil, ErrDisabled
	}
	if now.Sub(t.LastUsedAt) >= tokenTouchInterval {
		if err := s.db.TouchToken(ctx, t.ID, user.ID); err != nil {
			log.Warn().Err(err).Msg("Failed to record the use of a token")
		}
	}
	return &Principal{User: user, Token: t, Admin: s.IsAdmin(user.GitHubID)}, nil
}

// WriteInvalidToken answers 401 to a request with a bad bearer token, with
// the WWW-Authenticate header that says so. json picks the /api/v1 error
// form; otherwise the {"message"} form of the paste routes.
func WriteInvalidToken(w http.ResponseWriter, json bool) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="hastebin", error="invalid_token"`)
	if json {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "The API token is invalid or revoked.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// A paste route has set its own, stricter one already
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"message": "The API token is invalid or revoked."}` + "\n"))
}

// ClearSession tells the browser to drop its session cookie
func (s *Service) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
}

// UploadSession returns the principal of the session cookie of an upload,
// but only when the request also passes the CSRF check: X-CSRF-Token
// holds the session's token, Origin, when sent, is this server, and the
// request is not cross-site. The body may be anything. Otherwise the
// upload is anonymous, as it always was, and nil comes back.
func (s *Service) UploadSession(r *http.Request) (*Principal, error) {
	if r.Header.Get("X-CSRF-Token") == "" {
		return nil, nil
	}
	p, err := s.Session(r)
	if err != nil || p == nil {
		return nil, err
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return nil, nil
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(r, origin) {
		return nil, nil
	}
	if !p.ValidCSRF(r.Header.Get("X-CSRF-Token")) {
		return nil, nil
	}
	return p, nil
}
