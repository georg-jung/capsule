package app

import "testing"

func TestConfigRequiresAnExactSecurePublicOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		origin     string
		wantRPID   string
		wantOrigin string
		wantError  bool
	}{
		{name: "production HTTPS", origin: "https://tools.example.com", wantRPID: "tools.example.com", wantOrigin: "https://tools.example.com"},
		{name: "canonicalize host and default HTTPS port", origin: "HTTPS://TOOLS.Example.COM:443/", wantRPID: "tools.example.com", wantOrigin: "https://tools.example.com"},
		{name: "HTTPS with port", origin: "https://tools.example.com:8443", wantRPID: "tools.example.com", wantOrigin: "https://tools.example.com:8443"},
		{name: "localhost development", origin: "http://localhost:8080", wantRPID: "localhost", wantOrigin: "http://localhost:8080"},
		{name: "canonicalize localhost and default HTTP port", origin: "HTTP://LOCALHOST:80", wantRPID: "localhost", wantOrigin: "http://localhost"},
		{name: "loopback development", origin: "http://127.0.0.1:8080", wantRPID: "127.0.0.1", wantOrigin: "http://127.0.0.1:8080"},
		{name: "missing", origin: "", wantError: true},
		{name: "insecure production", origin: "http://tools.example.com", wantError: true},
		{name: "path", origin: "https://tools.example.com/capsule", wantError: true},
		{name: "credentials", origin: "https://user:secret@tools.example.com", wantError: true},
		{name: "query", origin: "https://tools.example.com?forwarded=yes", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := ParseConfig(func(key string) string {
				if key == "CAPSULE_ORIGIN" {
					return test.origin
				}
				return ""
			})
			if test.wantError {
				if err == nil {
					t.Fatalf("ParseConfig(%q) succeeded", test.origin)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RPID != test.wantRPID || cfg.Origin != test.wantOrigin {
				t.Fatalf("config = %#v", cfg)
			}
		})
	}
}
