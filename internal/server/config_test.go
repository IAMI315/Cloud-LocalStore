package server

import "testing"

func TestValidateConfigListenAddr(t *testing.T) {
	base := Config{DataDir: t.TempDir(), DatabasePath: ":memory:"}
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{name: "default loopback IPv4", addr: "127.0.0.1:8080"},
		{name: "localhost", addr: "localhost:8080"},
		{name: "loopback IPv6", addr: "[::1]:8080"},
		{name: "non-loopback IPv4", addr: "192.0.2.10:8080"},
		{name: "non-loopback IPv6", addr: "[2001:db8::10]:8080"},
		{name: "IPv4 wildcard", addr: "0.0.0.0:8080"},
		{name: "IPv6 wildcard", addr: "[::]:8080"},
		{name: "empty address", addr: "", wantErr: true},
		{name: "empty host", addr: ":8080", wantErr: true},
		{name: "missing port", addr: "127.0.0.1", wantErr: true},
		{name: "non-numeric port", addr: "127.0.0.1:http", wantErr: true},
		{name: "port out of range", addr: "127.0.0.1:65536", wantErr: true},
		{name: "arbitrary hostname", addr: "example.com:8080", wantErr: true},
		{name: "unbracketed IPv6", addr: "2001:db8::10:8080", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			cfg.ListenAddr = tt.addr
			err := ValidateConfig(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateConfig(%q) error = %v, wantErr=%v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

func TestIsLoopbackListenAddr(t *testing.T) {
	for _, tt := range []struct {
		addr string
		want bool
	}{
		{addr: "127.0.0.1:8080", want: true},
		{addr: "localhost:8080", want: true},
		{addr: "[::1]:8080", want: true},
		{addr: "0.0.0.0:8080", want: false},
		{addr: "192.0.2.10:8080", want: false},
		{addr: "[2001:db8::10]:8080", want: false},
	} {
		if got := IsLoopbackListenAddr(tt.addr); got != tt.want {
			t.Errorf("IsLoopbackListenAddr(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
