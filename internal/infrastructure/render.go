package infrastructure

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Dir is the deploy repo's folder for infrastructure repos: one folder per
// repo, which the forge-infrastructure ApplicationSet syncs.
const Dir = "infrastructure"

// RecordFile is the file in a repo's folder that says what Forge set up.
const RecordFile = "target.yaml"

// Record is RecordFile: the repo's target and environments. It lives in the
// deploy repo, not in the repo's forge.yaml, so editing the repo cannot
// move it to another account.
type Record struct {
	Target       string   `yaml:"target"`
	Team         string   `yaml:"team"`
	Environments []string `yaml:"environments"`
}

// Repo is one infrastructure repo to set up.
type Repo struct {
	Org, Name, Team string
	// Environments are the chosen environments of the target.
	Environments []string
}

// Folder is the deploy repo folder of the repo called name.
func Folder(name string) string { return Dir + "/" + name }

// RecordPath is the path of the repo's Record in the deploy repo.
func RecordPath(name string) string { return Folder(name) + "/" + RecordFile }

// Files renders what the deploy repo holds for r: its Record and one file
// of Crossplane objects per environment, by path.
func (c *Config) Files(t Target, r Repo) (map[string][]byte, error) {
	if len(r.Environments) == 0 {
		return nil, errors.New("choose at least one environment")
	}
	rec, err := marshal(Record{Target: t.Name, Team: r.Team, Environments: r.Environments})
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{RecordPath(r.Name): rec}
	for _, env := range r.Environments {
		e, ok := t.Environments[env]
		if !ok {
			return nil, fmt.Errorf("target %s has no %s (it has %s)", t.Name, env, strings.Join(t.EnvironmentNames(), ", "))
		}
		data, err := c.objects(e, r, env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		files[Folder(r.Name)+"/"+env+".yaml"] = data
	}
	return files, nil
}

// objects are one environment's Crossplane objects, as YAML documents.
func (c *Config) objects(e Environment, r Repo, env string) ([]byte, error) {
	base := r.Name + "-" + env
	labels := map[string]string{"forge.dev/service": r.Name, "forge.dev/deploy-env": env}
	ref := map[string]string{"kind": c.ProviderConfig.Kind, "name": c.ProviderConfig.Name}
	obj := func(apiVersion, kind, name string, forProvider map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "labels": labels},
			"spec":       map[string]any{"providerConfigRef": ref, "forProvider": forProvider},
		}
	}
	at := func(extra map[string]any) map[string]any {
		m := map[string]any{"repository": r.Name, "environment": env}
		maps.Copy(m, extra)
		return m
	}

	envSpec := at(map[string]any{"canAdminsBypass": false})
	if env == Production {
		// Only main may use production: the repo's other branches cannot
		// reach its secrets.
		envSpec["deploymentBranchPolicy"] = []any{map[string]any{"protectedBranches": false, "customBranchPolicies": true}}
	}
	docs := []map[string]any{obj("repo.github.m.upbound.io/v1alpha1", "Environment", base, envSpec)}
	if env == Production {
		docs = append(docs, obj("repo.github.m.upbound.io/v1alpha1", "EnvironmentDeploymentPolicy", base+"-main", at(map[string]any{"branchPattern": "main"})))
	}
	for _, name := range slices.Sorted(maps.Keys(e.GitHubSecrets)) {
		docs = append(docs, obj("actions.github.m.upbound.io/v1alpha1", "EnvironmentSecret", base+"-"+objName(name), at(map[string]any{
			"secretName":              name,
			"plaintextValueSecretRef": map[string]string{"name": e.Secret, "key": e.GitHubSecrets[name]},
		})))
	}
	for _, name := range slices.Sorted(maps.Keys(e.GitHubVariables)) {
		docs = append(docs, obj("actions.github.m.upbound.io/v1alpha1", "EnvironmentVariable", base+"-"+objName(name), at(map[string]any{
			"variableName": name,
			"value":        e.GitHubVariables[name],
		})))
	}

	var out bytes.Buffer
	for _, d := range docs {
		b, err := marshal(d)
		if err != nil {
			return nil, err
		}
		out.WriteString("---\n")
		out.Write(b)
	}
	if e.access != nil {
		access, err := e.renderAccess(AccessValues{Org: r.Org, Repo: r.Name, Team: r.Team, Environment: env, Subject: c.Subject(r.Org, r.Name, env)}, labels)
		if err != nil {
			return nil, fmt.Errorf("access: %w", err)
		}
		out.Write(access)
	}
	return out.Bytes(), nil
}

// renderAccess fills in the access template and checks it is Kubernetes
// objects, each with a kind and a name. It returns them as YAML documents,
// with labels added.
func (e *Environment) renderAccess(v AccessValues, labels map[string]string) ([]byte, error) {
	var b bytes.Buffer
	if err := e.access.Execute(&b, v); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	dec := yaml.NewDecoder(&b)
	for n := 1; ; n++ {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("object %d is not YAML: %w", n, err)
		}
		if doc == nil {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		if s, _ := doc["apiVersion"].(string); s == "" {
			return nil, fmt.Errorf("object %d has no apiVersion", n)
		}
		if s, _ := doc["kind"].(string); s == "" {
			return nil, fmt.Errorf("object %d has no kind", n)
		}
		if s, _ := meta["name"].(string); s == "" {
			return nil, fmt.Errorf("object %d has no metadata.name", n)
		}
		if _, ok := meta["namespace"]; ok {
			return nil, fmt.Errorf("object %d sets metadata.namespace; the objects go into the infrastructure namespace", n)
		}
		if len(labels) > 0 {
			l, _ := meta["labels"].(map[string]any)
			if l == nil {
				l = map[string]any{}
			}
			for k, v := range labels {
				l[k] = v
			}
			meta["labels"] = l
		}
		data, err := marshal(doc)
		if err != nil {
			return nil, err
		}
		out.WriteString("---\n")
		out.Write(data)
	}
	if out.Len() == 0 {
		return nil, errors.New("makes no objects")
	}
	return out.Bytes(), nil
}

// objName turns a GitHub secret or variable name into part of a Kubernetes
// name: AZURE_CLIENT_ID becomes azure-client-id.
func objName(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", "-")
}

// marshal writes v as YAML indented by two spaces, as Kubernetes manifests
// usually are.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
