package dashboard

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/armbian/ansi-hastebin/internal/accounts"
)

const (
	// MaxTitle is the longest title, in characters
	MaxTitle = 200

	// MaxLanguage is the longest language name
	MaxLanguage = 32

	// MaxTags is how many tags a paste may carry
	MaxTags = 10

	// MaxTokenName is the longest token name, in characters
	MaxTokenName = 64
)

var (
	tagPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	languagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+#._-]{0,31}$`)
)

// cleanText trims s and checks it is valid UTF-8 without control
// characters, at most limit characters
func cleanText(s string, limit int, what string) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return "", errors.New(what + " must be valid UTF-8.")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", errors.New(what + " must not hold control characters.")
		}
	}
	if utf8.RuneCountInString(s) > limit {
		return "", errors.New(what + " is too long.")
	}
	return s, nil
}

// NormalizeTitle checks a paste title: at most 200 characters, trimmed
func NormalizeTitle(s string) (string, error) {
	return cleanText(s, MaxTitle, "The title")
}

// NormalizeLanguage checks a language name: lowercase letters, digits and
// + # . _ -, at most 32, or empty for none
func NormalizeLanguage(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s != "" && !languagePattern.MatchString(s) {
		return "", errors.New("The language must be up to 32 letters, digits or + # . _ - characters.")
	}
	return s, nil
}

// NormalizeTags trims and lowercases tags, drops empty ones and duplicates,
// and checks the rest: [a-z0-9][a-z0-9._-]{0,31}, at most 10
func NormalizeTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] {
			continue
		}
		if !tagPattern.MatchString(tag) {
			return nil, errors.New("Tags are 1 to 32 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit.")
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > MaxTags {
		return nil, errors.New("A paste may carry at most 10 tags.")
	}
	return out, nil
}

// SplitTags splits header values like "a,b,c" into tags
func SplitTags(values []string) []string {
	var tags []string
	for _, value := range values {
		tags = append(tags, strings.Split(value, ",")...)
	}
	return tags
}

// ParseVisibility turns "link" or "private" into its stored value
func ParseVisibility(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "link":
		return accounts.VisibilityLink, nil
	case "private":
		return accounts.VisibilityPrivate, nil
	}
	return 0, errors.New(`The visibility must be "link" or "private".`)
}

// VisibilityName is the API form of a stored visibility
func VisibilityName(v int) string {
	if v == accounts.VisibilityPrivate {
		return "private"
	}
	return "link"
}

// ParseExpiry parses an RFC 3339 time that must be after now
func ParseExpiry(s string, now time.Time) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, errors.New("The expiry must be an RFC 3339 time, for example 2030-01-02T15:04:05Z.")
	}
	if !t.After(now) {
		return time.Time{}, errors.New("The expiry must be in the future.")
	}
	return t.UTC().Truncate(time.Second), nil
}
