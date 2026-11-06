package configs

import (
	"testing"

	"github.com/goccy/go-yaml"
)

func TestPublicReadOnlyServerConfig(t *testing.T) {
	var c ServerConfig
	if err := yaml.Unmarshal([]byte("use: os/main\nread_only: true\nallowed_stores: [_]\n"), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	if !c.ReadOnly || len(c.AllowedStores) != 1 || c.AllowedStores[0] != "_" {
		t.Fatalf("read-only namespace policy lost during config decoding: %+v", c)
	}
}
