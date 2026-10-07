// Package config loads the user config file and resolves the effective
// settings for one invocation. Precedence, highest first: flags, FORGE_*
// environment variables, the repo manifest, the user config file, built-in
// defaults. Credentials never live in this file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk user config, normally ~/.config/forge/config.yaml.
type Config struct {
	CurrentContext string      `yaml:"current-context,omitempty"`
	Contexts       []Context   `yaml:"contexts,omitempty"`
	Preferences    Preferences `yaml:"preferences,omitempty"`
}

// Context is one Forge installation and its defaults.
type Context struct {
	Name      string `yaml:"name"`
	API       string `yaml:"api"`
	Namespace string `yaml:"namespace,omitempty"`
}

// Preferences are presentation defaults.
type Preferences struct {
	Output string `yaml:"output,omitempty"`
	Color  string `yaml:"color,omitempty"`
}

// Env looks up an environment variable. os.LookupEnv in production; a map in tests.
type Env func(key string) (string, bool)

// DefaultPath returns $XDG_CONFIG_HOME/forge/config.yaml, falling back to
// ~/.config/forge/config.yaml on every OS so the location is predictable.
func DefaultPath(env Env) (string, error) {
	if p, ok := env("FORGE_CONFIG"); ok && p != "" {
		return p, nil
	}
	if dir, ok := env("XDG_CONFIG_HOME"); ok && dir != "" {
		return filepath.Join(dir, "forge", "config.yaml"), nil
	}
	home, ok := env("HOME")
	if !ok || home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".config", "forge", "config.yaml"), nil
}

// Load reads the config at path. A missing file is an empty config, not an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}

// Validate checks that context names are unique and current-context exists.
func (c *Config) Validate() error {
	seen := map[string]bool{}
	for _, ctx := range c.Contexts {
		if ctx.Name == "" {
			return errors.New("context with empty name")
		}
		if seen[ctx.Name] {
			return fmt.Errorf("duplicate context %q", ctx.Name)
		}
		seen[ctx.Name] = true
	}
	if c.CurrentContext != "" && !seen[c.CurrentContext] {
		return fmt.Errorf("current-context %q is not defined", c.CurrentContext)
	}
	return nil
}

// Context returns the named context.
func (c *Config) Context(name string) (Context, bool) {
	i := slices.IndexFunc(c.Contexts, func(x Context) bool { return x.Name == name })
	if i < 0 {
		return Context{}, false
	}
	return c.Contexts[i], true
}

// SetContext adds the context or replaces the one with the same name.
func (c *Config) SetContext(ctx Context) {
	i := slices.IndexFunc(c.Contexts, func(x Context) bool { return x.Name == ctx.Name })
	if i < 0 {
		c.Contexts = append(c.Contexts, ctx)
		return
	}
	c.Contexts[i] = ctx
}
