package bridgecontrol

import (
	"errors"
	"net/url"
	"strings"
)

// ValidateEndpoint enforces the same wss://-only shape at every point a
// control endpoint value is accepted: enrollment write time (cmd/bridgectl)
// and daemon read time (internal/localserver). A hand-edited or otherwise
// stale config could supply a plain ws:// URL; bridgectl must never send
// the bri_ bearer credential over an unencrypted connection, so both paths
// reject anything but a wss:// URL with a path and no credentials, query,
// or fragment, matching Bridge's own validation of BRIDGE_CONTROL_URL.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path == "" {
		return errors.New("control endpoint must be a wss:// URL with a path and no credentials, query, or fragment")
	}
	return nil
}
