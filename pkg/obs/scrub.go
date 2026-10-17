package obs

import (
	"net/url"
	"regexp"
)

var (
	urlRe     = regexp.MustCompile(`(?:https?|mxc|wss?)://\S+`)
	sipPassRe = regexp.MustCompile(`sips?:([^:@\s]+):[^@\s]+@`)
	bearerRe  = regexp.MustCompile(`(?i)bearer\s+\S+`)
	jwtRe     = regexp.MustCompile(`eyJ[\w-]+\.[\w-]+\.[\w-]*`)
	tokenKVRe = regexp.MustCompile(`(?i)(access_token|password|secret|token)=[^&\s]+`)
)

// stripURLs replaces every http(s), ws(s) or mxc URL in s with scheme://host.
func stripURLs(s string) string {
	return urlRe.ReplaceAllStringFunc(s, func(m string) string {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" {
			return "<url>"
		}
		return u.Scheme + "://" + u.Host
	})
}

// Scrub removes secrets from s (TELEMETRY-SPEC §6.3): URLs are reduced to scheme://host, SIP URI
// passwords, Bearer tokens, JWTs and token-like key=value pairs are masked.
func Scrub(s string) string {
	s = stripURLs(s)
	s = sipPassRe.ReplaceAllString(s, "sip:$1:***@")
	s = bearerRe.ReplaceAllString(s, "Bearer ***")
	s = jwtRe.ReplaceAllString(s, "<jwt>")
	return tokenKVRe.ReplaceAllString(s, "$1=***")
}
