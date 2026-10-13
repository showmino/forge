package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
)

func newServiceOptionsCmd(f *Factory) *cobra.Command {
	cmd := group("options", "Check and change a service's run options (replicas, secrets, jobs) in forge.yaml")
	cmd.AddCommand(newServiceOptionsCheckCmd(f), newServiceOptionsSetCmd(f))
	return cmd
}

// optionsView is what forge service options check prints as JSON.
type optionsView struct {
	Staging    envOptions `json:"staging"`
	Production envOptions `json:"production"`
	*manifest.Run
}

type envOptions struct {
	Replicas       int `json:"replicas"`
	MaxAutoscaling int `json:"maxAutoscaling,omitempty"`
}

func newServiceOptionsCheckCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "check [dir]",
		Short: "Check forge.yaml's run options and show what each environment gets, without the API",
		Long: `Check the run options in the forge.yaml of the repo in dir (default: the
current directory) by the rules the Forge API deploys with, and show what
staging and production get: staging runs one pod, production run.replicas
(grown up to run.autoscaling.max on CPU). Which 1Password vaults are allowed
is the API's to say, when it deploys. It needs no API and no login.`,
		Example: `  forge service options check
  forge service options check ../payments -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			wd, err := f.Getwd()
			if err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			path := filepath.Join(absFrom(wd, dir), manifest.FileName)
			data, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				return exitcode.Usagef("%s does not exist", path)
			}
			if err != nil {
				return err
			}
			m, err := manifest.Parse(data)
			if err != nil {
				return exitcode.New(exitcode.Unexpected, "%s: %v", manifest.FileName, err)
			}
			run := m.Run
			if run == nil {
				run = &manifest.Run{}
			}
			view := optionsView{Staging: envOptions{Replicas: 1}, Production: envOptions{Replicas: m.ProductionReplicas()}, Run: run}
			prod := fmt.Sprint(view.Production.Replicas)
			if a := run.Autoscaling; a != nil {
				view.Production.MaxAutoscaling = a.Max
				prod = fmt.Sprintf("%d to %d (on CPU)", view.Production.Replicas, a.Max)
			}
			rows := [][]string{{"replicas", "1", prod}}
			both := func(option, value string) { rows = append(rows, []string{option, value, value}) }
			if r := run.Resources; r != nil {
				both("resources", resourcesText(r))
			}
			if len(run.Env) > 0 {
				both("env", strings.Join(slices.Sorted(maps.Keys(run.Env)), ", "))
			}
			for _, s := range run.Secrets {
				both("secret "+s.Name, strings.Join(slices.Sorted(maps.Keys(s.Env)), ", ")+" from "+s.Item)
			}
			for _, j := range run.Jobs {
				both("job "+j.Name, j.Schedule+": "+strings.Join(j.Command, " "))
			}
			names := make([]string, len(rows))
			for i, r := range rows {
				names[i] = r[0]
			}
			if err := output.Print(f.IO.Out, format, output.View{Headers: []string{"option", "staging", "production"}, Rows: rows, Names: names, Data: view}); err != nil {
				return err
			}
			f.infof("%s's run options are valid.", m.Service.Name)
			return nil
		},
	}
}

func resourcesText(r *manifest.Resources) string {
	var parts []string
	for _, q := range []struct {
		what string
		q    *manifest.Quantities
	}{{"requests", r.Requests}, {"limits", r.Limits}} {
		if q.q == nil {
			continue
		}
		var s []string
		if q.q.CPU != "" {
			s = append(s, "cpu "+q.q.CPU)
		}
		if q.q.Memory != "" {
			s = append(s, "memory "+q.q.Memory)
		}
		parts = append(parts, q.what+" "+strings.Join(s, ", "))
	}
	return strings.Join(parts, "; ")
}
