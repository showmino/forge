package resource

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// planDoc is the part of `tofu show -json` Forge reads.
type planDoc struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Change  struct {
			Actions []string  `json:"actions"`
			Before  *planSide `json:"before"`
			After   *planSide `json:"after"`
		} `json:"change"`
	} `json:"resource_changes"`
}

type planSide struct {
	Manifest *struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Instances json.Number `json:"instances"`
			Storage   struct {
				Size string `json:"size"`
			} `json:"storage"`
		} `json:"spec"`
	} `json:"manifest"`
}

// database returns the name and settings of s when it is a CloudNativePG cluster.
func (s *planSide) database() (string, *forgev1.ResourceSettings) {
	if s == nil || s.Manifest == nil || s.Manifest.APIVersion != "postgresql.cnpg.io/v1" || s.Manifest.Kind != "Cluster" {
		return "", nil
	}
	m := s.Manifest
	gi, _ := strconv.ParseInt(strings.TrimSuffix(m.Spec.Storage.Size, "Gi"), 10, 32)
	n, _ := strconv.ParseInt(m.Spec.Instances.String(), 10, 32)
	return m.Metadata.Name, &forgev1.ResourceSettings{Name: m.Metadata.Name, Type: TypePostgres, StorageGi: int32(gi), Instances: int32(n)}
}

// action names what a plan's action list does; "" for nothing.
func action(actions []string) string {
	has := func(a string) bool { return slices.Contains(actions, a) }
	switch {
	case has("create") && has("delete"):
		return "replace"
	case has("create"):
		return "create"
	case has("delete"):
		return "delete"
	case has("update"):
		return "update"
	case len(actions) == 0, has("no-op"), has("read"):
		return ""
	default:
		return strings.Join(actions, "+")
	}
}

// Changes reads what a plan (the output of `tofu show -json`) would do,
// sorted by resource and then action. Changes that do nothing are left out.
func Changes(planJSON []byte) ([]*forgev1.ResourceChange, error) {
	if len(planJSON) == 0 {
		return nil, errors.New("the plan is empty")
	}
	var doc planDoc
	dec := json.NewDecoder(strings.NewReader(string(planJSON)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the plan is not readable: %w", err)
	}
	var out []*forgev1.ResourceChange
	for _, rc := range doc.ResourceChanges {
		act := action(rc.Change.Actions)
		if act == "" {
			continue
		}
		c := &forgev1.ResourceChange{Action: act, Resource: rc.Address, Type: rc.Type}
		beforeName, before := rc.Change.Before.database()
		afterName, after := rc.Change.After.database()
		if before != nil || after != nil {
			c.Type, c.Before, c.After = TypePostgres, before, after
			if c.Resource = afterName; c.Resource == "" {
				c.Resource = beforeName
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *forgev1.ResourceChange) int {
		if c := strings.Compare(a.GetResource(), b.GetResource()); c != 0 {
			return c
		}
		return strings.Compare(a.GetAction(), b.GetAction())
	})
	return out, nil
}

// Count sums changes up: "create=1 update=0 delete=0", with replace=N when N > 0.
func Count(changes []*forgev1.ResourceChange) string {
	n := map[string]int{}
	for _, c := range changes {
		n[c.GetAction()]++
	}
	s := fmt.Sprintf("create=%d update=%d delete=%d", n["create"], n["update"], n["delete"])
	if n["replace"] > 0 {
		s += fmt.Sprintf(" replace=%d", n["replace"])
	}
	return s
}

// Destructive reports whether any change deletes or replaces a resource.
func Destructive(changes []*forgev1.ResourceChange) bool {
	return slices.ContainsFunc(changes, func(c *forgev1.ResourceChange) bool {
		return c.GetAction() == "delete" || c.GetAction() == "replace"
	})
}
