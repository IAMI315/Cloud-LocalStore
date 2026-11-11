package server

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	DataDir, DatabasePath, ListenAddr, PairingToken string
	DevMode                                         bool
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{DataDir: "data", DatabasePath: "data/metadata.db", ListenAddr: "127.0.0.1:8080"}
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
		case "listen_addr":
			cfg.ListenAddr = strings.TrimSpace(parts[1])
		case "pairing_token":
			cfg.PairingToken = strings.TrimSpace(parts[1])
		case "dev_mode":
			cfg.DevMode = strings.EqualFold(strings.TrimSpace(parts[1]), "true")
		}
	}
	if err := ValidateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ValidateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.DataDir) == "" || strings.TrimSpace(cfg.DatabasePath) == "" {
		return fmt.Errorf("data_dir and database_path are required")
	}
	if filepath.IsAbs(cfg.DataDir) == false {
		cfg.DataDir = filepath.Clean(cfg.DataDir)
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return fmt.Errorf("listen_addr is required")
	}
	host, err := listenHost(cfg.ListenAddr)
	if err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return nil
	}
	return fmt.Errorf("invalid listen_addr: host must be localhost or a valid IPv4/IPv6 address")
}

func listenHost(addr string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", fmt.Errorf("invalid listen_addr: %w", err)
	}
	if host == "" {
		return "", fmt.Errorf("invalid listen_addr: host is required")
	}
	if port == "" {
		return "", fmt.Errorf("invalid listen_addr: port is required")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("invalid listen_addr: port must be between 0 and 65535")
	}
	return host, nil
}

// IsLoopbackListenAddr reports whether addr binds only to the local machine.
// It is used for startup diagnostics; address validation remains in ValidateConfig.
func IsLoopbackListenAddr(addr string) bool {
	host, err := listenHost(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
