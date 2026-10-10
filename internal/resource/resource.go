// Package resource holds the rules for the resources a service declares in
// forge.yaml: what is valid, what a declaration means in one environment,
// and what an OpenTofu plan would change.
package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// TypePostgres is the one resource type of this version.
const TypePostgres = "postgres"

// Defaults and limits of a declaration. Policy sets the storage limits.
const (
	defaultStorageGi = 1
	maxInstances     = 3
)

var envs = []string{"staging", "production"}

var (
	nameRE    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)
	storageRE = regexp.MustCompile(`^[1-9][0-9]{0,5}Gi$`)
)

func defaultInstances(env string) int32 {
	if env == "production" {
		return 2
	}
	return 1
}

// storageGi reads "<n>Gi"; "" is the default.
func storageGi(s string) (int32, error) {
	if s == "" {
		return defaultStorageGi, nil
	}
	if !storageRE.MatchString(s) {
		return 0, fmt.Errorf("storage %q must be whole gibibytes such as 5Gi", s)
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(s, "Gi"), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("storage %q must be whole gibibytes such as 5Gi", s)
	}
	return int32(n), nil
}

// instanceCount checks n; 0 is "use the default".
func instanceCount(n int32) error {
	if n < 0 || n > maxInstances {
		return fmt.Errorf("instances %d must be 1 to %d", n, maxInstances)
	}
	return nil
}

// Validate checks decls for every environment.
func Validate(decls []*forgev1.ResourceDeclaration) error {
	for _, env := range envs {
		if _, err := Resolve(decls, env); err != nil {
			return err
		}
	}
	return nil
}

// Resolve checks decls and returns their settings in env, sorted by name.
// Every value is checked, also those env does not use.
func Resolve(decls []*forgev1.ResourceDeclaration, env string) ([]*forgev1.ResourceSettings, error) {
	if !slices.Contains(envs, env) {
		return nil, fmt.Errorf("environment %q must be staging or production", env)
	}
	seen := map[string]bool{}
	postgres := 0
	out := make([]*forgev1.ResourceSettings, 0, len(decls))
	for i, d := range decls {
		if d == nil {
			return nil, fmt.Errorf("entry %d is empty", i+1)
		}
		name := d.GetName()
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("name %q must be a lowercase DNS label of at most 30 characters", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("name %q is used twice", name)
		}
		seen[name] = true
		if d.GetType() != TypePostgres {
			return nil, fmt.Errorf("%s: type %q must be postgres", name, d.GetType())
		}
		if postgres++; postgres > 1 {
			return nil, errors.New("only one postgres resource per service is supported")
		}
		gi, err := storageGi(d.GetStorage())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := instanceCount(d.GetInstances()); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		instances := d.GetInstances()
		for _, k := range slices.Sorted(maps.Keys(d.GetEnvironments())) {
			if !slices.Contains(envs, k) {
				return nil, fmt.Errorf("%s: environments.%s must be staging or production", name, k)
			}
			o := d.GetEnvironments()[k]
			if o == nil {
				continue
			}
			ogi, err := storageGi(o.GetStorage())
			if err != nil {
				return nil, fmt.Errorf("%s: environments.%s: %w", name, k, err)
			}
			if err := instanceCount(o.GetInstances()); err != nil {
				return nil, fmt.Errorf("%s: environments.%s: %w", name, k, err)
			}
			if k != env {
				continue
			}
			if o.GetStorage() != "" {
				gi = ogi
			}
			if o.GetInstances() != 0 {
				instances = o.GetInstances()
			}
		}
		if instances == 0 {
			instances = defaultInstances(env)
		}
		out = append(out, &forgev1.ResourceSettings{Name: name, Type: TypePostgres, StorageGi: gi, Instances: instances})
	}
	slices.SortFunc(out, func(a, b *forgev1.ResourceSettings) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, nil
}

// Digest identifies a list of settings: equal lists have equal digests.
func Digest(settings []*forgev1.ResourceSettings) string {
	h := sha256.New()
	for _, s := range settings {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", s.GetName(), s.GetType(), s.GetStorageGi(), s.GetInstances())
	}
	return hex.EncodeToString(h.Sum(nil))
}
