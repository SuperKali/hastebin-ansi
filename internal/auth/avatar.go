package auth

import (
	"net/url"
	"strings"
)

const (
	// AvatarHost is the only host avatars are loaded from; the CSP img-src
	// names it too
	AvatarHost = "avatars.githubusercontent.com"

	// maxAvatarURL is the longest avatar address kept
	maxAvatarURL = 512

	// avatarSize is the pixel size asked of GitHub: 24 px at up to 2x, and
	// the 40 px picture in the account menu
	avatarSize = "64"
)

// CheckAvatarURL returns the avatar address GitHub sent when it is
// https://avatars.githubusercontent.com/..., in a normalized form, else "".
// The host must match exactly (no other case, no userinfo, no port but 443),
// so nothing else can end up in an <img src> on the pages.
func CheckAvatarURL(raw string) string {
	if raw == "" || len(raw) > maxAvatarURL || strings.ContainsAny(raw, "\\ \t\r\n") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil {
		return ""
	}
	// url.Parse lowers the scheme only, so the host is compared as sent
	if u.Scheme != "https" || u.Hostname() != AvatarHost {
		return ""
	}
	if port := u.Port(); port != "" && port != "443" {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") || len(u.Path) < 2 {
		return ""
	}
	u.Host = AvatarHost
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// sizedAvatar returns a stored avatar address with s=64 set, nil when the
// user has none. The address is checked again, so a row edited by hand
// cannot put another host on the page.
func sizedAvatar(stored string) any {
	clean := CheckAvatarURL(stored)
	if clean == "" {
		return nil
	}
	u, err := url.Parse(clean)
	if err != nil {
		return nil
	}
	query := u.Query()
	query.Set("s", avatarSize)
	u.RawQuery = query.Encode()
	return u.String()
}
