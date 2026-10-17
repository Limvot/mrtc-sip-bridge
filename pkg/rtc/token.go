package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

const (
	sfuGetPath       = "/sfu/get"
	maxTokenRespSize = 1 << 20
	maxErrBodyBytes  = 300
)

// tokenStatusError is returned by fetchToken for a non-200 answer. Body is at most maxErrBodyBytes of the
// (non-secret, Matrix-style JSON) error body, already scrubbed.
type tokenStatusError struct {
	Status int
	Body   string
}

func (e *tokenStatusError) Error() string {
	return fmt.Sprintf("lk-jwt-service %s returned %d: %s", sfuGetPath, e.Status, e.Body)
}

type sfuRequest struct {
	Room        string            `json:"room"`
	OpenIDToken *core.OpenIDToken `json:"openid_token"`
	DeviceID    string            `json:"device_id"`
}

type sfuResponse struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
}

// fetchToken asks lk-jwt-service for a LiveKit URL and JWT (wire §4.4). Neither the OpenID access token
// nor the JWT ever appears in a returned error.
func fetchToken(ctx context.Context, hc *http.Client, serviceURL string, roomID id.RoomID, deviceID string, tok *core.OpenIDToken) (url, jwt string, err error) {
	body, err := json.Marshal(sfuRequest{Room: string(roomID), OpenIDToken: tok, DeviceID: deviceID})
	if err != nil {
		return "", "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serviceURL, "/")+sfuGetPath, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("build request: %s", obs.Scrub(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("POST %s: %s", sfuGetPath, obs.Scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenRespSize))
	if err != nil {
		return "", "", fmt.Errorf("read response: %s", obs.Scrub(err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		b := raw
		if len(b) > maxErrBodyBytes {
			b = b[:maxErrBodyBytes]
		}
		return "", "", &tokenStatusError{Status: resp.StatusCode, Body: obs.Scrub(string(b))}
	}
	var out sfuResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.URL == "" || out.JWT == "" {
		return "", "", fmt.Errorf("lk-jwt-service response not understood")
	}
	return out.URL, out.JWT, nil
}
