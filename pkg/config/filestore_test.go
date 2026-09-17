package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSecretStoreRoundTripsAndKeepsTheFilePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	store := NewFileSecretStore(path)

	if _, err := store.Get("auth_token:1"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing file: err = %v, want ErrSecretNotFound", err)
	}
	if err := store.Set("auth_token:1", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("ct0:1", "csrf"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o, want 0600", info.Mode().Perm())
	}
	if value, err := store.Get("ct0:1"); err != nil || value != "csrf" {
		t.Fatalf("Get = %q, %v", value, err)
	}
	if err := store.Delete("ct0:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("ct0:1"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("after delete: err = %v", err)
	}
	if err := store.Delete("never-set"); err != nil {
		t.Fatalf("deleting an absent key must be a no-op, got %v", err)
	}
}

func TestFileSecretStoreDecodesMacOSKeychainExports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	contents := "\"auth_token:1\": \"go-keyring-base64:dG9r\"\n\"ct0:1\": \"go-keyring-encoded:63737266\"\n\"plain:1\": \"asis\"\n\"bad:1\": \"go-keyring-base64:@@\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileSecretStore(path)
	for key, want := range map[string]string{"auth_token:1": "tok", "ct0:1": "csrf", "plain:1": "asis"} {
		if got, err := store.Get(key); err != nil || got != want {
			t.Fatalf("Get(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
	if _, err := store.Get("bad:1"); err == nil {
		t.Fatal("a malformed keychain export must not be returned as-is")
	}
}

func TestFileSecretStoreRefusesWorldReadableFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose.yaml")
	if err := os.WriteFile(loose, []byte("auth_token:1: tok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewFileSecretStore(loose).Get("auth_token:1")
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable file: err = %v", err)
	}

	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("auth_token:1: tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileSecretStore(link).Get("auth_token:1"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: err = %v", err)
	}
	if err := NewFileSecretStore(link).Set("auth_token:1", "x"); err == nil {
		t.Fatal("Set through a symlink must be refused")
	}
}

func TestFileSecretStoreWorksAsTheConfigManagerKeyring(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".xeet.yaml"), []byte("version: 2\nactive: \"1\"\naccounts:\n  \"1\":\n    handle: alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileSecretStore(filepath.Join(dir, "secrets.yaml"))
	if err := store.Set("auth_token:1", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("ct0:1", "csrf"); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewConfigManagerAt(dir, store).LoadAccount("1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "tok" || cfg.CT0 != "csrf" || cfg.Handle != "alice" {
		t.Fatalf("cfg = %+v", cfg)
	}
}
