package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeTags(t *testing.T) {
	tags, err := NormalizeTags([]string{" Build ", "armbian", "build", "", "  ", "v1.2_rc-3"})
	require.NoError(t, err)
	require.Equal(t, []string{"build", "armbian", "v1.2_rc-3"}, tags)

	tags, err = NormalizeTags(nil)
	require.NoError(t, err)
	require.Equal(t, []string{}, tags)

	for _, bad := range []string{"-lead", ".dot", "has space", "semi;colon", "ünï", strings.Repeat("a", 33), "a/b", "#tag"} {
		_, err := NormalizeTags([]string{bad})
		require.Error(t, err, bad)
	}
	_, err = NormalizeTags([]string{strings.Repeat("a", 32)})
	require.NoError(t, err)

	ten := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	_, err = NormalizeTags(ten)
	require.NoError(t, err)
	_, err = NormalizeTags(append(ten, "k"))
	require.Error(t, err)
	// Duplicates do not count against the limit
	_, err = NormalizeTags(append(ten, "A", "b "))
	require.NoError(t, err)

	require.Equal(t, []string{"a", "b", " c", "d"}, SplitTags([]string{"a,b", " c,d"}))
}

func TestNormalizeText(t *testing.T) {
	title, err := NormalizeTitle("  build log  ")
	require.NoError(t, err)
	require.Equal(t, "build log", title)
	_, err = NormalizeTitle(strings.Repeat("é", 200))
	require.NoError(t, err)
	_, err = NormalizeTitle(strings.Repeat("é", 201))
	require.Error(t, err)
	_, err = NormalizeTitle("a\x00b")
	require.Error(t, err)
	_, err = NormalizeTitle("a\nb")
	require.Error(t, err)
	_, err = NormalizeTitle("\xff")
	require.Error(t, err)

	lang, err := NormalizeLanguage(" C++ ")
	require.NoError(t, err)
	require.Equal(t, "c++", lang)
	lang, err = NormalizeLanguage("")
	require.NoError(t, err)
	require.Empty(t, lang)
	for _, bad := range []string{"java script", "<b>", strings.Repeat("x", 33)} {
		_, err := NormalizeLanguage(bad)
		require.Error(t, err, bad)
	}
}

func TestVisibilityAndExpiry(t *testing.T) {
	v, err := ParseVisibility("Private")
	require.NoError(t, err)
	require.Equal(t, "private", VisibilityName(v))
	v, err = ParseVisibility("link")
	require.NoError(t, err)
	require.Equal(t, "link", VisibilityName(v))
	_, err = ParseVisibility("public")
	require.Error(t, err)

	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	exp, err := ParseExpiry("2026-10-29T12:00:00+02:00", now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), exp)
	for _, bad := range []string{"2026-10-28T12:00:00Z", "2020-01-01T00:00:00Z", "tomorrow", "2026-10-29"} {
		_, err := ParseExpiry(bad, now)
		require.Error(t, err, bad)
	}
}

func TestParseUploadHeaders(t *testing.T) {
	now := time.Now()
	r := httptest.NewRequest("POST", "/log", nil)
	r.Header.Set("X-Paste-Title", " my log ")
	r.Header.Add("X-Paste-Tags", "Build, armbian")
	r.Header.Add("X-Paste-Tags", "rk3588")
	r.Header.Set("X-Paste-Visibility", "private")
	r.Header.Set("X-Paste-Expires", now.Add(time.Hour).UTC().Format(time.RFC3339))
	meta, err := parseUploadHeaders(r, 0, now)
	require.NoError(t, err)
	require.Equal(t, "my log", meta.title)
	require.Equal(t, []string{"build", "armbian", "rk3588"}, meta.tags)
	require.Equal(t, 1, meta.visibility)
	require.False(t, meta.expires.IsZero())

	// Without headers: the user's default visibility, nothing else
	meta, err = parseUploadHeaders(httptest.NewRequest("POST", "/log", nil), 1, now)
	require.NoError(t, err)
	require.Equal(t, 1, meta.visibility)
	require.Empty(t, meta.title)
	require.Empty(t, meta.tags)
	require.True(t, meta.expires.IsZero())

	for name, value := range map[string]string{
		"X-Paste-Visibility": "public",
		"X-Paste-Expires":    "2001-01-01T00:00:00Z",
		"X-Paste-Tags":       "bad tag",
		"X-Paste-Title":      strings.Repeat("t", 201),
	} {
		r := httptest.NewRequest("POST", "/log", nil)
		r.Header.Set(name, value)
		_, err := parseUploadHeaders(r, 0, now)
		require.Error(t, err, name)
	}
}
