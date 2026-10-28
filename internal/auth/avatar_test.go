package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckAvatarURL(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"https://avatars.githubusercontent.com/u/1001?v=4", "https://avatars.githubusercontent.com/u/1001?v=4"},
		{"https://avatars.githubusercontent.com/u/1001", "https://avatars.githubusercontent.com/u/1001"},
		{"HTTPS://avatars.githubusercontent.com/u/1?v=4", "https://avatars.githubusercontent.com/u/1?v=4"},
		{"https://avatars.githubusercontent.com:443/u/1?v=4", "https://avatars.githubusercontent.com/u/1?v=4"},
		{"https://avatars.githubusercontent.com/u/1?v=4#frag", "https://avatars.githubusercontent.com/u/1?v=4"},

		// Not GitHub's avatar host, or not https
		{"", ""},
		{"http://avatars.githubusercontent.com/u/1", ""},
		{"https://evil.com/u/1", ""},
		{"https://evil.com/avatars.githubusercontent.com", ""},
		{"https://avatars.githubusercontent.com.evil.com/u/1", ""},
		{"https://evilavatars.githubusercontent.com/u/1", ""},
		{"https://gravatar.com/avatar/1", ""},
		{"https://github.com/octocat.png", ""},
		{"//avatars.githubusercontent.com/u/1", ""},
		{"avatars.githubusercontent.com/u/1", ""},
		{"/u/1", ""},
		{"javascript:alert(1)", ""},
		{"javascript://avatars.githubusercontent.com/%0aalert(1)", ""},
		{"data:image/png;base64,iVBORw0KGgo=", ""},
		{"data://avatars.githubusercontent.com/u/1", ""},
		{"ftp://avatars.githubusercontent.com/u/1", ""},

		// Userinfo tricks: the real host is after the @
		{"https://avatars.githubusercontent.com@evil.com/u/1", ""},
		{"https://user:pass@avatars.githubusercontent.com/u/1", ""},
		{"https://avatars.githubusercontent.com%40evil.com/u/1", ""},

		// Ports other than the default, and hosts not spelled exactly
		{"https://avatars.githubusercontent.com:8443/u/1", ""},
		{"https://avatars.githubusercontent.com:80/u/1", ""},
		{"https://avatars.githubusercontent.com:/u/1", "https://avatars.githubusercontent.com/u/1"},
		{"https://AVATARS.GITHUBUSERCONTENT.COM/u/1", ""},
		{"https://Avatars.GitHubUserContent.com/u/1", ""},
		{"https://avatars.githubusercontent.com./u/1", ""},

		// No picture path, odd characters, too long
		{"https://avatars.githubusercontent.com", ""},
		{"https://avatars.githubusercontent.com/", ""},
		{"https://avatars.githubusercontent.com\\@evil.com/u/1", ""},
		{"https://avatars.githubusercontent.com/u/1 onerror=x", ""},
		{"https://avatars.githubusercontent.com/u/1\n", ""},
		{"https://avatars.githubusercontent.com/u/" + string(make([]byte, 600)), ""},
	} {
		require.Equal(t, tt.want, CheckAvatarURL(tt.in), tt.in)
	}
}

func TestSizedAvatar(t *testing.T) {
	require.Equal(t, "https://avatars.githubusercontent.com/u/1?s=64&v=4", sizedAvatar("https://avatars.githubusercontent.com/u/1?v=4"))
	require.Equal(t, "https://avatars.githubusercontent.com/u/1?s=64", sizedAvatar("https://avatars.githubusercontent.com/u/1"))
	require.Equal(t, "https://avatars.githubusercontent.com/u/1?s=64&v=4", sizedAvatar("https://avatars.githubusercontent.com/u/1?s=460&v=4"))
	require.Nil(t, sizedAvatar(""))
	// A row changed by hand still cannot put another host on the page
	require.Nil(t, sizedAvatar("https://evil.com/u/1"))
}

func TestSessionAvatar(t *testing.T) {
	h := newHarness(t)
	c := h.browser()

	// The fake GitHub user has an avatar: it is stored and sent sized
	require.Equal(t, http.StatusSeeOther, h.login(c, "").StatusCode)
	user, _ := h.session(c)
	require.Equal(t, "https://avatars.githubusercontent.com/u/1001?s=64&v=4", user["avatar_url"])
	stored, err := h.db.UserByGitHubID(context.Background(), 1001)
	require.NoError(t, err)
	require.Equal(t, "https://avatars.githubusercontent.com/u/1001?v=4", stored.AvatarURL)

	// One on another host is dropped at the next login, the old one too
	h.gh.setUser(githubAccount{ID: 1001, Login: "octocat", CreatedAt: time.Now().Add(-1000 * time.Hour),
		AvatarURL: "https://avatars.githubusercontent.com@evil.com/u/1001"})
	h.login(c, "")
	user, _ = h.session(c)
	require.Contains(t, user, "avatar_url")
	require.Nil(t, user["avatar_url"])
	var avatar *string
	require.NoError(t, h.db.R.QueryRow("SELECT avatar_url FROM users WHERE github_id = 1001").Scan(&avatar))
	require.Nil(t, avatar)

	// A user without an avatar gets null, a new account with a bad one too
	for i, bad := range []string{"", "javascript:alert(1)", "http://avatars.githubusercontent.com/u/9", "data:image/png;base64,AA=="} {
		other := h.browser()
		h.gh.setUser(githubAccount{ID: int64(2000 + i), Login: "plain", CreatedAt: time.Now().Add(-1000 * time.Hour), AvatarURL: bad})
		h.login(other, "")
		user, _ = h.session(other)
		require.Equal(t, "plain", user["login"], bad)
		require.Nil(t, user["avatar_url"], bad)
	}
}
