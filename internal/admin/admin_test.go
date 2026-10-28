package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestFilePastesScannedAtMostEveryTenMinutes(t *testing.T) {
	store := storage.NewFileStorage(t.TempDir(), 0).(*storage.FileStorage)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Set("one", "12345", false))

	h := &Handler{Files: store}
	start := time.Unix(1_800_000_000, 0)
	fp := h.filePastes(start)
	require.EqualValues(t, 1, fp.Count)
	require.EqualValues(t, 5, fp.Bytes)
	require.Equal(t, start.UTC(), fp.AsOf)

	// A new paste shows only once the cached count is ten minutes old
	require.NoError(t, store.Set("two", "123", false))
	fp = h.filePastes(start.Add(9 * time.Minute))
	require.EqualValues(t, 1, fp.Count)
	require.Equal(t, start.UTC(), fp.AsOf)
	fp = h.filePastes(start.Add(10 * time.Minute))
	require.EqualValues(t, 2, fp.Count)
	require.EqualValues(t, 8, fp.Bytes)
	require.Equal(t, start.Add(10*time.Minute).UTC(), fp.AsOf)
}

func TestParseTarget(t *testing.T) {
	const base = "https://paste.example.com"
	for _, c := range []struct{ in, want string }{
		{"abcdefghij", "abcdefghij"},
		{"  abcdefghij  ", "abcdefghij"},
		{"abcdefghij.log", "abcdefghij"},
		{"abcdefghij.r3", "abcdefghij"},
		{"abcdefghij?x=1", "abcdefghij"},
		{"abcdefghij#top", "abcdefghij"},
		{"/abcdefghij", "abcdefghij"},
		{"/raw/abcdefghij.js?download=1", "abcdefghij"},
		{"https://paste.example.com/abcdefghij", "abcdefghij"},
		{"https://PASTE.example.com/abcdefghij.sh", "abcdefghij"},
		{"http://paste.example.com/raw/abcdefghij", "abcdefghij"},
		{"https://paste.example.com/documents/abcdefghij?x=1#y", "abcdefghij"},
		{"https://paste.example.com/dashboard/p/abcdefghij", "abcdefghij"},
		{"paste.example.com/abcdefghij", "abcdefghij"},
		{"https://paste.example.com/abcdefghij/", "abcdefghij"},
	} {
		got, err := ParseTarget(c.in, base)
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got, c.in)
	}
	for _, in := range []string{
		"", " ", "https://evil.example.com/abcdefghij", "evil.example.com/abcdefghij",
		"https://paste.example.com.evil.com/abcdefghij", "https://paste.example.com@evil.com/abcdefghij",
		"https://user@paste.example.com/abcdefghij", "//evil.example.com/abcdefghij",
		"ftp://paste.example.com/abcdefghij", "javascript:alert(1)", "https://paste.example.com/",
		"https://paste.example.com/admin/users", "https://paste.example.com/raw/a/b", "/api/v1/pastes/abc",
		"abc def", `abc\def`, "../etc/passwd", "a%2Fb", strings.Repeat("a", 65), strings.Repeat("a", 3000),
	} {
		_, err := ParseTarget(in, base)
		require.Error(t, err, in)
	}

	// Without a base URL any host is taken, still only for paste paths
	got, err := ParseTarget("https://anything.example/raw/abcdefghij", "")
	require.NoError(t, err)
	require.Equal(t, "abcdefghij", got)
}

func TestCleanReason(t *testing.T) {
	got, err := cleanReason("  spam link  ")
	require.NoError(t, err)
	require.Equal(t, "spam link", got)
	for _, in := range []string{"", "   ", "bad\x00", "line\nbreak", string([]byte{0xff, 0xfe}), strings.Repeat("é", MaxReason+1)} {
		_, err := cleanReason(in)
		require.Error(t, err, in)
	}
	_, err = cleanReason(strings.Repeat("é", MaxReason))
	require.NoError(t, err)
}

func TestKeyHash(t *testing.T) {
	require.Equal(t, "#"+"2c26b46b", KeyHash("foo"))
}
