package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// FileSecretStore keeps the session cookies in one YAML file instead of the
// OS keyring. It exists for headless hosts (a VPS running `xeet mcp serve`)
// where no Secret Service is available and the boundary is the OS user and
// the file mode instead. It refuses files that are readable by anyone else
// or that are symlinks, because either would silently widen that boundary.
//
// The file is a flat map of keyring key to value, e.g.
//
//	auth_token:1234: ...
//	ct0:1234: ...
type FileSecretStore struct {
	path string
	mu   sync.Mutex
}

// NewFileSecretStore returns a store backed by path. The file need not exist
// yet; reads of a missing file behave as an empty store.
func NewFileSecretStore(path string) *FileSecretStore {
	return &FileSecretStore{path: path}
}

// Path returns where the store reads and writes.
func (s *FileSecretStore) Path() string { return s.path }

func (s *FileSecretStore) read() (map[string]string, error) {
	info, err := os.Lstat(s.path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("secrets file %s is a symlink; refusing to read it", s.path)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("secrets file %s is mode %04o; it must not be readable by group or others (chmod 600)", s.path, mode)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return values, nil
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parsing secrets file %s: %w", s.path, err)
	}
	return values, nil
}

func (s *FileSecretStore) write(values map[string]string) error {
	if fi, err := os.Lstat(s.path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("secrets file %s is a symlink; refusing to replace it", s.path)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(values))
	for _, key := range keys {
		ordered[key] = values[key]
	}
	data, err := yaml.Marshal(ordered)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".xeet-secrets-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (s *FileSecretStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return "", err
	}
	value, ok := values[key]
	if !ok || value == "" {
		return "", ErrSecretNotFound
	}
	return decodeKeychainExport(value)
}

// go-keyring stores every macOS Keychain value wrapped as
// "go-keyring-base64:<base64>" (older versions: "go-keyring-encoded:<hex>"),
// and `security find-generic-password -w` hands that wrapper back verbatim.
// Accepting it here means a headless host can be provisioned by piping that
// command's output straight into the file, with no decoding step that would
// have to handle the secret in a shell.
const (
	keychainBase64Prefix = "go-keyring-base64:"
	keychainHexPrefix    = "go-keyring-encoded:"
)

func decodeKeychainExport(value string) (string, error) {
	switch {
	case strings.HasPrefix(value, keychainBase64Prefix):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[len(keychainBase64Prefix):]))
		if err != nil {
			return "", fmt.Errorf("secret is marked as a keychain export but is not valid base64: %w", err)
		}
		return string(decoded), nil
	case strings.HasPrefix(value, keychainHexPrefix):
		decoded, err := hex.DecodeString(strings.TrimSpace(value[len(keychainHexPrefix):]))
		if err != nil {
			return "", fmt.Errorf("secret is marked as a keychain export but is not valid hex: %w", err)
		}
		return string(decoded), nil
	}
	return value, nil
}

func (s *FileSecretStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		return errors.New("secret key is empty")
	}
	values, err := s.read()
	if err != nil {
		return err
	}
	values[key] = value
	return s.write(values)
}

func (s *FileSecretStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := values[key]; !ok {
		return nil
	}
	delete(values, key)
	return s.write(values)
}
