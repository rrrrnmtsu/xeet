package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSecretsRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".xeet.secrets")
	f := &fileSecrets{path: path}
	if _, err := f.Get("auth_token:1"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing key should be ErrSecretNotFound, got %v", err)
	}
	if err := f.Set("auth_token:1", "abc"); err != nil {
		t.Fatal(err)
	}
	if v, err := f.Get("auth_token:1"); err != nil || v != "abc" {
		t.Fatalf("got %q, %v", v, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file mode %v, want 0600", info.Mode().Perm())
	}
	if err := f.Delete("auth_token:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("auth_token:1"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("deleted key still readable: %v", err)
	}
}
