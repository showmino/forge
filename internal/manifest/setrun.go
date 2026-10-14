package manifest

import (
	"bytes"
	"errors"
	"slices"

	"gopkg.in/yaml.v3"
)

// Options are the run options a template can offer in its create form, in
// the order forge.yaml has them.
var Options = []string{"replicas", "resources", "env", "secrets", "jobs", "autoscaling"}

// ValidOption reports whether name is one of Options.
func ValidOption(name string) bool { return slices.Contains(Options, name) }

// OptionsSet names the options r sets, in the order of Options.
func (r *Run) OptionsSet() []string {
	if r == nil {
		return nil
	}
	set := map[string]bool{
		"replicas": r.Replicas != 0, "resources": r.Resources != nil, "env": len(r.Env) > 0,
		"secrets": len(r.Secrets) > 0, "jobs": len(r.Jobs) > 0, "autoscaling": r.Autoscaling != nil,
	}
	var out []string
	for _, o := range Options {
		if set[o] {
			out = append(out, o)
		}
	}
	return out
}

// SetRunOptions writes r's options into the forge.yaml in data, replacing
// the ones there and removing those r leaves empty. Everything else in the
// file, its comments included, stays as it was.
func SetRunOptions(data []byte, r *Run) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("forge.yaml is not a mapping")
	}
	root := doc.Content[0]
	run := mapValue(root, "run")
	if run == nil {
		run = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "run"}, run)
	}
	values := map[string]any{
		"replicas": r.Replicas, "resources": r.Resources, "env": r.Env,
		"secrets": r.Secrets, "jobs": r.Jobs, "autoscaling": r.Autoscaling,
	}
	set := r.OptionsSet()
	for _, key := range Options {
		removeKey(run, key)
		if !slices.Contains(set, key) {
			continue
		}
		var v yaml.Node
		if err := v.Encode(values[key]); err != nil {
			return nil, err
		}
		run.Content = append(run.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &v)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// SetRunDomain sets run.domain in the forge.yaml in data ("none": only the
// internal address), keeping everything else and its comments.
func SetRunDomain(data []byte, domain string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("forge.yaml is not a mapping")
	}
	root := doc.Content[0]
	run := mapValue(root, "run")
	if run == nil {
		run = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "run"}, run)
	}
	run.Style = 0
	removeKey(run, "domain")
	run.Content = append(run.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "domain"}, &yaml.Node{Kind: yaml.ScalarNode, Value: domain})
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func removeKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
