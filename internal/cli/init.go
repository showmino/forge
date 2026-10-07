package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
)

func newInitCmd(f *Factory) *cobra.Command {
	var name, team, lifecycle string
	var owners []string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a forge.yaml for the repo in the current directory",
		Long: `Write a forge.yaml manifest for an existing repo. The service name defaults
to the directory name. Register it afterwards with 'forge service register'.`,
		Example: `  forge init --team billing
  forge init --name payments --team billing --owner ana@example.com --lifecycle production`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wd, err := f.Getwd()
			if err != nil {
				return err
			}
			path := filepath.Join(wd, manifest.FileName)
			if _, err := os.Stat(path); err == nil && !force {
				return exitcode.New(exitcode.Conflict, "%s already exists", path).WithHint("pass --force to overwrite it")
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if name == "" {
				name = slug(filepath.Base(wd))
			}
			if team == "" {
				if !f.interactive() {
					return exitcode.Usagef("--team is required")
				}
				fmt.Fprint(f.IO.ErrOut, "Owning team: ")
				line, _ := bufio.NewReader(f.IO.In).ReadString('\n')
				team = strings.TrimSpace(line)
			}
			m := &manifest.Manifest{
				APIVersion: manifest.APIVersion,
				Service:    manifest.Service{Name: name, Team: team, Owners: owners, Lifecycle: lifecycle},
			}
			if err := m.Validate(); err != nil {
				return exitcode.Usagef("%v", err)
			}
			data, err := m.Marshal()
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // forge.yaml is committed to the repo
				return err
			}
			f.infof("Wrote %s. Register the service with 'forge service register'.", path)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&name, "name", "", "service name (default: directory name)")
	fl.StringVar(&team, "team", "", "owning team")
	fl.StringSliceVar(&owners, "owner", nil, "owner email or handle (repeatable)")
	fl.StringVar(&lifecycle, "lifecycle", "experimental", "experimental, production or deprecated")
	fl.BoolVar(&force, "force", false, "overwrite an existing forge.yaml")
	return cmd
}

var nonSlug = regexp.MustCompile(`[^a-z0-9-]+`)

// slug turns a directory name into a service id: "My_Service" -> "my-service".
func slug(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	s = strings.TrimLeft(s, "0123456789-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}
