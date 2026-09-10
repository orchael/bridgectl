package config

import "testing"

func TestRequiresNodeRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{
			name: "no providers configured",
			cfg:  &Config{},
			want: false,
		},
		{
			name: "empty providers map",
			cfg: &Config{
				Providers: map[string]ProviderConfig{},
			},
			want: false,
		},
		{
			name: "node binary",
			cfg: &Config{
				Providers: map[string]ProviderConfig{
					"codex": {Binary: "node"},
				},
			},
			want: true,
		},
		{
			name: "native go binary (agy)",
			cfg: &Config{
				Providers: map[string]ProviderConfig{
					"gemini": {Binary: "agy"},
				},
			},
			want: false,
		},
		{
			name: "non-node native binary",
			cfg: &Config{
				Providers: map[string]ProviderConfig{
					"fixture": {Binary: "/bin/cat"},
				},
			},
			want: false,
		},
		{
			name: "native cli via bin shim",
			cfg: &Config{
				Providers: map[string]ProviderConfig{
					"opencode": {Binary: "/opt/bridgectl/node_modules/.bin/opencode"},
				},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RequiresNodeRuntime(tc.cfg); got != tc.want {
				t.Fatalf("RequiresNodeRuntime() = %v, want %v", got, tc.want)
			}
		})
	}
}
