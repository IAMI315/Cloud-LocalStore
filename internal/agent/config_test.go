package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("root_path: C:/Sync\ndatabase_path: state.db\nspace_id: home\n"), 0o600); err != nil { t.Fatal(err) }
	cfg, err := LoadConfig(path)
	if err != nil { t.Fatal(err) }
	if cfg.RootPath != "C:/Sync" || cfg.DatabasePath != "state.db" || cfg.SpaceID != "home" { t.Fatalf("unexpected config: %#v", cfg) }
}
