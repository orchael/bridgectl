package bridgecontrol

import "testing"

func TestValidateEndpoint(t *testing.T) {
	valid := []string{
		"wss://control.bridge.orchael.dev/v1/control",
		"wss://127.0.0.1:8081/v1/control",
	}
	for _, raw := range valid {
		if err := ValidateEndpoint(raw); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want nil", raw, err)
		}
	}

	invalid := []string{
		"",
		"ws://control.bridge.orchael.dev/v1/control",
		"https://control.bridge.orchael.dev/v1/control",
		"wss://control.bridge.orchael.dev",                 // no path
		"wss://control.bridge.orchael.dev/v1/control?x=1",  // query
		"wss://user@control.bridge.orchael.dev/v1/control", // userinfo
		"wss://control.bridge.orchael.dev/v1/control#frag", // fragment
		"not a url at all",
	}
	for _, raw := range invalid {
		if err := ValidateEndpoint(raw); err == nil {
			t.Errorf("ValidateEndpoint(%q) = nil, want an error", raw)
		}
	}
}
