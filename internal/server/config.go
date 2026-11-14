package server

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultStorageQuotaBytes int64 = 5 * 1024 * 1024 * 1024
const maxStorageQuotaBytes int64 = 1 << 50

type Config struct {
	DataDir, DatabasePath, ListenAddr, PairingToken string
	DevMode                                         bool
	StorageQuotaBytes                               int64
}

// DefaultConfig returns the portable server defaults. The executable directory
// is used as the root so a downloaded binary can be copied as one folder and
// started without a separate installation step.
func DefaultConfig(baseDir string) Config {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = "."
	}
	baseDir = filepath.Clean(baseDir)
	dataDir := filepath.Join(baseDir, "data")
	return Config{DataDir: dataDir, DatabasePath: filepath.Join(dataDir, "metadata.db"), ListenAddr: "0.0.0.0:18080", StorageQuotaBytes: DefaultStorageQuotaBytes}
}

// LoadOrCreateConfig loads path, or creates a portable configuration when the
// path does not exist. Existing explicit configuration files are never
// overwritten.
func LoadOrCreateConfig(path, baseDir string) (Config, string, error) {
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(baseDir, "server.conf")
	}
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err == nil {
		cfg, err := LoadConfig(path)
		return cfg, path, err
	} else if !os.IsNotExist(err) {
		return Config{}, path, err
	}
	cfg := DefaultConfig(baseDir)
	if err := ValidateConfig(cfg); err != nil {
		return Config{}, path, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return Config{}, path, err
	}
	contents := fmt.Sprintf("# Cloud LocalStore portable server configuration\n# Generated automatically; edit only when needed.\ndata_dir: %s\ndatabase_path: %s\nlisten_addr: %s\nstorage_quota_bytes: %d\n", cfg.DataDir, cfg.DatabasePath, cfg.ListenAddr, cfg.StorageQuotaBytes)
	if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
		return Config{}, path, err
	}
	return cfg, path, nil
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{DataDir: "data", DatabasePath: "data/metadata.db", ListenAddr: "127.0.0.1:8080", StorageQuotaBytes: DefaultStorageQuotaBytes}
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
		case "storage_quota_bytes":
			value, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if err != nil {
				return Config{}, fmt.Errorf("invalid storage_quota_bytes")
			}
			cfg.StorageQuotaBytes = value
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
	if cfg.StorageQuotaBytes < 0 || cfg.StorageQuotaBytes > maxStorageQuotaBytes {
		return fmt.Errorf("storage_quota_bytes must be between 1 and %d", maxStorageQuotaBytes)
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
