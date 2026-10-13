package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
)

func newServiceOptionsSetCmd(f *Factory) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "set <key>=<value>...",
		Short: "Change run options in the repo's forge.yaml, without the API",
		Long: `Change run options in the forge.yaml of the repo in --dir (default: the
current directory), keeping the rest of the file and its comments. The keys:

  replicas                      production's pod count
  autoscaling.max               production grows on CPU up to this many pods
  resources.requests.cpu        and .memory, resources.limits.cpu and .memory
  env.<NAME>                    a plain environment variable

An empty value removes the key. The result must pass 'forge service options
check'; nothing is written otherwise. Secrets and jobs are lists: edit them in
forge.yaml. Push the change: it applies at the next deploy (staging) and
promote (production).`,
		Example: `  forge service options set replicas=3 autoscaling.max=6
  forge service options set env.LOG_LEVEL=debug resources.limits.memory=256Mi
  forge service options set autoscaling.max=`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			wd, err := f.Getwd()
			if err != nil {
				return err
			}
			path := filepath.Join(absFrom(wd, dir), manifest.FileName)
			data, err := os.ReadFile(path)
			if err != nil {
				return exitcode.Usagef("read %s: %v", path, err)
			}
			m, err := manifest.Parse(data)
			if err != nil {
				return exitcode.New(exitcode.Unexpected, "%s: %v", manifest.FileName, err)
			}
			run := manifest.Run{}
			if m.Run != nil {
				run = *m.Run
			}
			for _, kv := range args {
				key, value, ok := strings.Cut(kv, "=")
				if !ok {
					return exitcode.Usagef("%q must be <key>=<value>", kv)
				}
				if err := setRunKey(&run, key, value); err != nil {
					return err
				}
			}
			out, err := manifest.SetRunOptions(data, &run)
			if err != nil {
				return err
			}
			if _, err := manifest.Parse(out); err != nil {
				return exitcode.New(exitcode.Unexpected, "%v; %s is unchanged", err, manifest.FileName)
			}
			if err := os.WriteFile(path, out, 0o644); err != nil { //nolint:gosec // forge.yaml is a public file of the repo
				return err
			}
			f.infof("%s updated. Push it: staging gets it at its next deploy, production at its next promote.", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "the repo whose forge.yaml to change")
	return cmd
}

// setRunKey sets one key of forge service options set; "" removes it.
func setRunKey(r *manifest.Run, key, value string) error {
	number := func() (int, error) {
		if value == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, exitcode.Usagef("%s=%q must be a number", key, value)
		}
		return n, nil
	}
	switch {
	case key == "replicas":
		n, err := number()
		r.Replicas = n
		return err
	case key == "autoscaling.max":
		n, err := number()
		r.Autoscaling = nil
		if n != 0 {
			r.Autoscaling = &manifest.Autoscaling{Max: n}
		}
		return err
	case strings.HasPrefix(key, "env."):
		name := strings.TrimPrefix(key, "env.")
		if value == "" {
			delete(r.Env, name)
			return nil
		}
		env := map[string]string{name: value}
		for k, v := range r.Env {
			if k != name {
				env[k] = v
			}
		}
		r.Env = env
		return nil
	case strings.HasPrefix(key, "resources."):
		parts := strings.Split(key, ".")
		if len(parts) != 3 || (parts[1] != "requests" && parts[1] != "limits") || (parts[2] != "cpu" && parts[2] != "memory") {
			break
		}
		rs := manifest.Resources{}
		if r.Resources != nil {
			rs = *r.Resources
		}
		q := &rs.Requests
		if parts[1] == "limits" {
			q = &rs.Limits
		}
		next := manifest.Quantities{}
		if *q != nil {
			next = **q
		}
		if parts[2] == "cpu" {
			next.CPU = value
		} else {
			next.Memory = value
		}
		*q = &next
		if next == (manifest.Quantities{}) {
			*q = nil
		}
		r.Resources = &rs
		if rs.Requests == nil && rs.Limits == nil {
			r.Resources = nil
		}
		return nil
	}
	return exitcode.Usagef("%q is not a key forge service options set knows: replicas, autoscaling.max, resources.<requests|limits>.<cpu|memory>, env.<NAME>", key)
}
