package server

import (
	"fmt"
	"slices"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/template"
)

// runFromProto is the forge.yaml form of the create form's run options.
func runFromProto(ro *forgev1.RunOptions) *manifest.Run {
	r := &manifest.Run{Replicas: int(ro.GetReplicas())}
	if len(ro.GetEnv()) > 0 {
		r.Env = ro.GetEnv()
	}
	if res := ro.GetResources(); res != nil {
		q := func(cpu, memory string) *manifest.Quantities {
			if cpu == "" && memory == "" {
				return nil
			}
			return &manifest.Quantities{CPU: cpu, Memory: memory}
		}
		rs := &manifest.Resources{Requests: q(res.GetRequestsCpu(), res.GetRequestsMemory()), Limits: q(res.GetLimitsCpu(), res.GetLimitsMemory())}
		if rs.Requests != nil || rs.Limits != nil {
			r.Resources = rs
		}
	}
	for _, s := range ro.GetSecrets() {
		r.Secrets = append(r.Secrets, manifest.Secret{Name: s.GetName(), Item: s.GetItem(), Env: s.GetEnv()})
	}
	for _, j := range ro.GetJobs() {
		r.Jobs = append(r.Jobs, manifest.Job{Name: j.GetName(), Schedule: j.GetSchedule(), Command: j.GetCommand()})
	}
	if ro.GetAutoscalingMax() > 0 {
		r.Autoscaling = &manifest.Autoscaling{Max: int(ro.GetAutoscalingMax())}
	}
	return r
}

// applyRunOptions writes the create form's run options into the rendered
// forge.yaml, over the template's. It refuses an option the template does
// not offer, and options forge.yaml's rules refuse.
func applyRunOptions(files []template.File, t *template.Template, ro *forgev1.RunOptions) ([]template.File, error) {
	asked := runFromProto(ro)
	for _, o := range asked.OptionsSet() {
		if !slices.Contains(t.Options, o) {
			offered := "none"
			if len(t.Options) > 0 {
				offered = strings.Join(t.Options, ", ")
			}
			return nil, invalid(fmt.Sprintf("the template %s does not offer the run option %s (it offers: %s)", t.Name, o, offered))
		}
	}
	i := slices.IndexFunc(files, func(f template.File) bool { return f.Path == manifest.FileName })
	if i < 0 {
		return nil, invalid("the template renders no forge.yaml")
	}
	m, err := manifest.Parse(files[i].Data)
	if err != nil {
		return nil, invalid(err.Error())
	}
	run := manifest.Run{}
	if m.Run != nil {
		run = *m.Run
	}
	for _, o := range asked.OptionsSet() {
		switch o {
		case "replicas":
			run.Replicas = asked.Replicas
		case "resources":
			run.Resources = asked.Resources
		case "env":
			run.Env = asked.Env
		case "secrets":
			run.Secrets = asked.Secrets
		case "jobs":
			run.Jobs = asked.Jobs
		case "autoscaling":
			run.Autoscaling = asked.Autoscaling
		}
	}
	data, err := manifest.SetRunOptions(files[i].Data, &run)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if _, err := manifest.Parse(data); err != nil {
		return nil, invalid("run options: " + err.Error())
	}
	out := slices.Clone(files)
	out[i].Data = data
	return out, nil
}
