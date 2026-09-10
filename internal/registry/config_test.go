package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "test-config.yaml")

	content := `
listen: ":9443"
tls:
  ca_bundle: "/certs/ca.crt"
  cert: "/certs/server.crt"
  key: "/certs/server.key"
storage:
  type: "file"
  base_dir: "/data/keys"
servers:
  - id: "prod"
    allowed_issuers:
      - "client-a"
      - "client-b"
  - id: "dev"
    allowed_issuers:
      - "client-a"
`

	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := LoadConfig(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, ":9443", cfg.Listen)
	assert.Equal(t, "/certs/ca.crt", cfg.TLS.CABundle)
	assert.Equal(t, "file", cfg.Storage.Type)
	assert.Equal(t, "/data/keys", cfg.Storage.BaseDir)
	assert.Len(t, cfg.Servers, 2)
}

func TestLoadConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "minimal.yaml")

	content := `
servers:
  - id: "s1"
    allowed_issuers: ["i1"]
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := LoadConfig(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, ":8443", cfg.Listen)
	assert.Equal(t, "memory", cfg.Storage.Type)
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.yaml")
	assert.Error(t, err)
}

func TestBuildServerConfig(t *testing.T) {
	cfg := &Config{
		Servers: []ServerEntry{
			{ID: "s1", AllowedIssuers: []string{"a", "b"}},
			{ID: "s2", AllowedIssuers: []string{"c"}},
		},
	}

	sc := cfg.BuildServerConfig()
	assert.Len(t, sc.AllowedIssuers, 2)
	assert.Equal(t, []string{"a", "b"}, sc.AllowedIssuers["s1"])
	assert.Equal(t, []string{"c"}, sc.AllowedIssuers["s2"])
}
