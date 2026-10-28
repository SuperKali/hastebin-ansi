package auth

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/stretchr/testify/require"
)

func TestTokenFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok := NewToken()
		require.Len(t, tok, TokenLen)
		require.True(t, strings.HasPrefix(tok, "hbp_"))
		require.True(t, ValidTokenFormat(tok), tok)
		require.False(t, seen[tok])
		seen[tok] = true
		require.Equal(t, tok[:10], TokenDisplay(tok))
	}
	require.Equal(t, 53, TokenLen)

	tok := NewToken()
	// One changed character anywhere is caught by the checksum
	for _, i := range []int{4, 20, len(tok) - 7, len(tok) - 1} {
		b := []byte(tok)
		if b[i] == 'a' {
			b[i] = 'b'
		} else {
			b[i] = 'a'
		}
		require.False(t, ValidTokenFormat(string(b)), "position %d", i)
	}
	for _, bad := range []string{"", "hbp_", tok[:len(tok)-1], tok + "a", "hbx_" + tok[4:], strings.Replace(tok, tok[10:11], "-", 1),
		"HBP_" + tok[4:]} {
		require.False(t, ValidTokenFormat(bad), bad)
	}

	// The hash is SHA-256 of the whole token, never the token
	require.Len(t, HashToken(tok), 32)
	require.NotEqual(t, HashToken(tok), HashToken(NewToken()))
}

// tokenService is a Service on a fresh database with a user and a token
func tokenService(t *testing.T) (*Service, *accounts.DB, *accounts.User, string, *accounts.Token) {
	t.Helper()
	db, err := accounts.Open(filepath.Join(t.TempDir(), "hastebin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc, err := New(db, Config{SessionTTL: time.Hour, SessionIdle: time.Hour, BaseURL: "https://paste.example"})
	require.NoError(t, err)
	user, _, err := db.LoginUser(context.Background(), 77, "tokenuser", "", true)
	require.NoError(t, err)
	tok := NewToken()
	row, err := db.CreateToken(context.Background(), user.ID, "test", TokenDisplay(tok), HashToken(tok), 20)
	require.NoError(t, err)
	return svc, db, user, tok, row
}

func TestBearer(t *testing.T) {
	svc, db, user, tok, row := tokenService(t)
	ctx := context.Background()

	bearer := func(header, query string) (*Principal, error) {
		r := httptest.NewRequest("POST", "/log"+query, nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		return svc.Bearer(r)
	}

	p, err := bearer("Bearer "+tok, "")
	require.NoError(t, err)
	require.Equal(t, user.ID, p.User.ID)
	require.Equal(t, row.ID, p.Token.ID)
	require.Nil(t, p.Session)
	p, err = bearer("bearer  "+tok+" ", "")
	require.NoError(t, err, "the scheme is case insensitive")
	require.NotNil(t, p)
	for _, h := range []string{"Bearer\t" + tok, "BEARER\t \t" + tok, "bEaReR\u00a0" + tok} {
		p, err = bearer(h, "")
		require.NoError(t, err, "any whitespace ends the scheme: %q", h)
		require.NotNil(t, p, h)
	}

	// No header, or another scheme: no principal, no error
	p, err = bearer("", "")
	require.NoError(t, err)
	require.Nil(t, p)
	p, err = bearer("Basic dXNlcjpwYXNz", "")
	require.NoError(t, err)
	require.Nil(t, p)

	// A token in the query string is never read
	for _, q := range []string{"?token=" + tok, "?access_token=" + tok, "?authorization=Bearer%20" + tok} {
		p, err = bearer("", q)
		require.NoError(t, err)
		require.Nil(t, p)
	}

	// Malformed, unknown, empty: invalid
	for _, h := range []string{"Bearer", "Bearer ", "Bearer x", "Bearer " + NewToken(), "Bearer " + tok[:len(tok)-1] + "0",
		"Bearer\tx", "bearer\t\t" + NewToken()} {
		_, err = bearer(h, "")
		require.ErrorIs(t, err, ErrInvalidToken, h)
	}

	// More than one Authorization header is no token, even when one is a
	// good Bearer: next to another Bearer, another scheme or an empty one,
	// in either order. None of them is picked or dropped.
	for _, pair := range [][2]string{
		{"Bearer " + tok, "Bearer " + NewToken()},
		{"Basic dXNlcjpwYXNz", "Bearer\t" + tok},
		{"", "Bearer " + tok},
		{"Bearer " + tok, ""},
		{"Basic dXNlcjpwYXNz", "Basic dXNlcjpwYXNz"},
	} {
		r := httptest.NewRequest("POST", "/log", nil)
		r.Header.Add("Authorization", pair[0])
		r.Header.Add("Authorization", pair[1])
		require.True(t, HasBearer(r), "%q", pair)
		p, err = svc.Bearer(r)
		require.ErrorIs(t, err, ErrInvalidToken, "%q", pair)
		require.Nil(t, p)
	}
	// One empty header is no token attempt, like no header
	r := httptest.NewRequest("POST", "/log", nil)
	r.Header.Add("Authorization", "")
	require.False(t, HasBearer(r))

	// The checksum is checked before any lookup: with the database closed a
	// malformed token is still just invalid, while a well formed one fails on the lookup
	db2, err := accounts.Open(filepath.Join(t.TempDir(), "closed.db"))
	require.NoError(t, err)
	closed, err := New(db2, Config{SessionTTL: time.Hour, SessionIdle: time.Hour})
	require.NoError(t, err)
	db2.Close()
	r = httptest.NewRequest("POST", "/log", nil)
	r.Header.Set("Authorization", "Bearer "+tok[:len(tok)-1]+"0")
	_, err = closed.Bearer(r)
	require.ErrorIs(t, err, ErrInvalidToken)
	r.Header.Set("Authorization", "Bearer "+NewToken())
	_, err = closed.Bearer(r)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidToken)

	// last_used_at is written at most once a minute
	used := func() int64 {
		got, _, err := db.TokenByHash(ctx, HashToken(tok))
		require.NoError(t, err)
		return got.LastUsedAt.Unix()
	}
	setUsed := func(at int64) {
		_, err := db.W.Exec("UPDATE api_tokens SET last_used_at = ? WHERE id = ?", at, row.ID)
		require.NoError(t, err)
	}
	recent := time.Now().Add(-30 * time.Second).Unix()
	setUsed(recent)
	_, err = bearer("Bearer "+tok, "")
	require.NoError(t, err)
	require.Equal(t, recent, used())
	setUsed(time.Now().Add(-2 * time.Minute).Unix())
	_, err = bearer("Bearer "+tok, "")
	require.NoError(t, err)
	require.GreaterOrEqual(t, used(), time.Now().Add(-5*time.Second).Unix())

	// Expired, disabled, revoked
	_, err = db.W.Exec("UPDATE api_tokens SET expires_at = ? WHERE id = ?", time.Now().Unix(), row.ID)
	require.NoError(t, err)
	_, err = bearer("Bearer "+tok, "")
	require.ErrorIs(t, err, ErrInvalidToken)
	_, err = db.W.Exec("UPDATE api_tokens SET expires_at = NULL WHERE id = ?", row.ID)
	require.NoError(t, err)
	require.NoError(t, db.SetDisabled(ctx, user.ID, true, "abuse"))
	_, err = bearer("Bearer "+tok, "")
	require.ErrorIs(t, err, ErrDisabled)
	require.NoError(t, db.SetDisabled(ctx, user.ID, false, ""))
	require.NoError(t, db.DeleteToken(ctx, user.ID, row.ID))
	_, err = bearer("Bearer "+tok, "")
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestWriteInvalidToken(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		rec := httptest.NewRecorder()
		WriteInvalidToken(rec, asJSON)
		require.Equal(t, 401, rec.Code)
		require.Equal(t, `Bearer realm="hastebin", error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
		if asJSON {
			require.Contains(t, rec.Body.String(), `"error":"unauthenticated"`)
		} else {
			require.Contains(t, rec.Body.String(), `"message"`)
		}
	}
}

func TestUploadSession(t *testing.T) {
	svc, db, user, _, _ := tokenService(t)
	ctx := context.Background()
	id, csrf := newSecret(), newSecret()
	require.NoError(t, db.CreateSession(ctx, hashSecret(id), user.ID, csrf, time.Now().Add(time.Hour)))

	send := func(cookie bool, headers ...string) (*Principal, error) {
		r := httptest.NewRequest("POST", "/documents", strings.NewReader("x"))
		if cookie {
			r.AddCookie(svc.cookie(sessionCookie, encode(id), 0))
		}
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		return svc.UploadSession(r)
	}
	upload := func(headers ...string) *Principal {
		p, err := send(true, headers...)
		require.NoError(t, err)
		return p
	}
	require.Nil(t, upload(), "a cookie alone is anonymous")
	p := upload("X-CSRF-Token", encode(csrf), "Origin", "https://paste.example")
	require.NotNil(t, p)
	require.Equal(t, user.ID, p.User.ID)
	require.NotNil(t, upload("X-CSRF-Token", encode(csrf), "Content-Type", "text/plain"), "any body type")

	// A CSRF token that does not pass is never anonymous
	for _, headers := range [][]string{
		{"X-CSRF-Token", encode(newSecret())},
		{"X-CSRF-Token", "garbage"},
		{"X-CSRF-Token", encode(csrf), "Origin", "https://evil.example"},
		{"X-CSRF-Token", encode(csrf), "Sec-Fetch-Site", "cross-site"},
	} {
		p, err := send(true, headers...)
		require.ErrorIs(t, err, ErrSessionExpired, headers)
		require.Nil(t, p)
	}
	// Nor is one without its session: no cookie, or the session ended
	_, err := send(false, "X-CSRF-Token", encode(csrf))
	require.ErrorIs(t, err, ErrSessionExpired)
	require.NoError(t, db.DeleteSession(ctx, hashSecret(id)))
	_, err = send(true, "X-CSRF-Token", encode(csrf))
	require.ErrorIs(t, err, ErrSessionExpired)
	p, err = send(true)
	require.NoError(t, err)
	require.Nil(t, p, "the stale cookie alone is still anonymous")
}
