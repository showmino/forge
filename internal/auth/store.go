package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
)

// ErrNotFound means no credentials are stored for the key.
var ErrNotFound = errors.New("no stored credentials")

// Store keeps credentials, keyed by API URL.
type Store interface {
	Get(key string) (*Credentials, error)
	Set(key string, c *Credentials) error
	Delete(key string) error
	// Name describes where credentials live, for 'forge auth status'.
	Name() string
}

const keyringService = "forge-cli"

// KeyringStore uses the OS keychain: macOS Keychain, Secret Service on Linux,
// Windows Credential Manager.
type KeyringStore struct{}

func (KeyringStore) Name() string { return "system keyring" }

func (KeyringStore) Get(key string) (*Credentials, error) {
	s, err := keyring.Get(keyringService, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, fmt.Errorf("decode stored credentials: %w", err)
	}
	return &c, nil
}

func (KeyringStore) Set(key string, c *Credentials) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return keyring.Set(keyringService, key, string(b))
}

func (KeyringStore) Delete(key string) error {
	err := keyring.Delete(keyringService, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// FileStore keeps credentials in a 0600 JSON file. It is the fallback for
// headless Linux without a Secret Service.
//
// TODO(phase-2): encrypt the file at rest, as the design doc specifies.
type FileStore struct {
	Path string
	mu   sync.Mutex
}

func (f *FileStore) Name() string { return "file " + f.Path }

func (f *FileStore) load() (map[string]*Credentials, error) {
	m := map[string]*Credentials{}
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", f.Path, err)
	}
	return m, nil
}

func (f *FileStore) save(m map[string]*Credentials) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

func (f *FileStore) Get(key string) (*Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return nil, err
	}
	c, ok := m[key]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func (f *FileStore) Set(key string, c *Credentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	m[key] = c
	return f.save(m)
}

func (f *FileStore) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return f.save(m)
}

// FallbackStore uses Primary and falls back to Secondary when Primary is
// unavailable (for example no D-Bus session on a CI runner).
type FallbackStore struct {
	Primary, Secondary Store
	once               sync.Once
	active             Store
}

func (s *FallbackStore) pick() Store {
	s.once.Do(func() {
		s.active = s.Primary
		if _, err := s.Primary.Get("forge-probe"); err != nil && !errors.Is(err, ErrNotFound) {
			s.active = s.Secondary
		}
	})
	return s.active
}

func (s *FallbackStore) Name() string                         { return s.pick().Name() }
func (s *FallbackStore) Get(key string) (*Credentials, error) { return s.pick().Get(key) }
func (s *FallbackStore) Set(key string, c *Credentials) error { return s.pick().Set(key, c) }
func (s *FallbackStore) Delete(key string) error              { return s.pick().Delete(key) }

// MemoryStore is an in-memory Store for tests.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]*Credentials
}

func (s *MemoryStore) Name() string { return "memory" }

func (s *MemoryStore) Get(key string) (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[key]
	if !ok {
		return nil, ErrNotFound
	}
	cc := *c
	return &cc, nil
}

func (s *MemoryStore) Set(key string, c *Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*Credentials{}
	}
	cc := *c
	s.m[key] = &cc
	return nil
}

func (s *MemoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}
