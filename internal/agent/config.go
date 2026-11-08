package agent

import (
	"fmt"
	"os"
	"strings"
)

type Config struct { RootPath, DatabasePath, SpaceID string }

func LoadConfig(path string) (Config, error) {
	cfg := Config{DatabasePath: "clouddrive.db", SpaceID: "local"}
	if path == "" { return cfg, nil }
	data, err := os.ReadFile(path); if err != nil { return Config{}, err }
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line); if line == "" || strings.HasPrefix(line, "#") { continue }
		parts := strings.SplitN(line, ":", 2); if len(parts) != 2 { return Config{}, fmt.Errorf("invalid config line %q", line) }
		switch strings.TrimSpace(parts[0]) { case "root_path": cfg.RootPath = strings.TrimSpace(parts[1]); case "database_path": cfg.DatabasePath = strings.TrimSpace(parts[1]); case "space_id": cfg.SpaceID = strings.TrimSpace(parts[1]) }
	}
	if cfg.RootPath == "" { return Config{}, fmt.Errorf("root_path is required") }
	return cfg, nil
}
