package telemetry

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

var segmentRe = regexp.MustCompile(`^(_matrix|v[0-9]{1,2}|r0|[a-z_]{1,32})$`)

// urlTemplate reduces a Matrix API path to a bounded template (ids become "*").
func urlTemplate(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p != "" && !segmentRe.MatchString(p) {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, "/")
}

// WrapMatrixTransport wraps base so requests made with a traced context get a client span and a
// traceparent header, and (when metrics are on) record http.client.request.duration. It is a thin
// wrapper over obs.WrapHTTPTransport with the Matrix path templater. A nil base means
// http.DefaultTransport.
func WrapMatrixTransport(base http.RoundTripper) http.RoundTripper {
	return obs.WrapHTTPTransport(base, urlTemplate)
}
