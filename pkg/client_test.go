package clutchcall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClientInitialization(t *testing.T) {
	c := &Client{
		endpoint: "quic://127.0.0.1:9090",
		creds: Credentials{
			TenantID: "mock-tenant",
		},
	}
	if c.creds.TenantID != "mock-tenant" {
		t.Errorf("Client initialization failed parameter assignment")
	}
}

func TestMethodIDConstants(t *testing.T) {
	cases := map[string]uint32{
		"ORIGINATE":      MethodID_ORIGINATE,
		"ORIGINATE_BULK": MethodID_ORIGINATE_BULK,
		"TERMINATE":      MethodID_TERMINATE,
		"AUDIO_FRAME":    MethodID_AUDIO_FRAME,
		"STREAM_EVENTS":  MethodID_STREAM_EVENTS,
	}
	expected := map[string]uint32{
		"ORIGINATE":      1430677891,
		"ORIGINATE_BULK": 721069100,
		"TERMINATE":      3834253405,
		"AUDIO_FRAME":    2991054320,
		"STREAM_EVENTS":  959835745,
	}
	for name, got := range cases {
		if got != expected[name] {
			t.Errorf("MethodID %s: got %d, want %d", name, got, expected[name])
		}
	}
}

func TestNewClient_ParsesCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	body := `{"tenant_id":"tenant-A","private_key":"pk","private_key_id":"kid"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	client, err := NewClient("quic://127.0.0.1:9090", path)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.creds.TenantID != "tenant-A" {
		t.Errorf("TenantID: got %q, want %q", client.creds.TenantID, "tenant-A")
	}
	if client.creds.PrivateKeyID != "kid" {
		t.Errorf("PrivateKeyID: got %q, want %q", client.creds.PrivateKeyID, "kid")
	}
	if client.clientId == "" {
		t.Errorf("clientId was not initialized")
	}
}

func TestNewClient_MissingFile(t *testing.T) {
	_, err := NewClient("quic://127.0.0.1:9090", "/nonexistent/path.json")
	if err == nil {
		t.Errorf("expected error for missing credentials file")
	}
}
