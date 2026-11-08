package server

import (
	"fmt"
	"os"
	"strings"
)

type Config struct{ DataDir, DatabasePath string }

func LoadConfig(path string) (Config, error) {
	cfg := Config{DataDir: "data", DatabasePath: "data/metadata.db"}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return Config{}, fmt.Errorf("invalid config line %q", line)
		}
		switch strings.TrimSpace(parts[0]) {
		case "data_dir":
			cfg.DataDir = strings.TrimSpace(parts[1])
		case "database_path":
			cfg.DatabasePath = strings.TrimSpace(parts[1])
		}
	}
	return cfg, nil
}
