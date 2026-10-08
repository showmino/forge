package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/template"
)

func newTemplateCmd(f *Factory) *cobra.Command {
	cmd := group("template", "Inspect and lint service templates", "templates")
	cmd.Long = `Templates scaffold new services with 'forge create'. A template is a directory
with a template.yaml (typed parameters) and a skeleton/ rendered with Go
text/template. The platform team releases them by tagging the template repo.`
	cmd.AddCommand(newTemplateLintCmd(f))
	return cmd
}

func newTemplateLintCmd(f *Factory) *cobra.Command {
	var renderDir string
	cmd := &cobra.Command{
		Use:   "lint [dir]",
		Short: "Check templates for errors, without the API",
		Long: `Check one template (a directory with a template.yaml) or every template in a
directory of templates (default: the current directory). Lint checks
template.yaml and its parameters, parses every skeleton file and path, renders
the template with sample values and checks the rendered forge.yaml. It needs
no API and no login.

Exits 0 when there are no problems and 1 when there are.`,
		Example: `  forge template lint                  # every template in the current directory
  forge template lint go-grpc
  forge template lint -o json | jq -r '.findings[].message'`,
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
			fsys, names, err := findTemplates(absFrom(wd, dir))
			if err != nil {
				return err
			}
			if renderDir != "" {
				renderDir = absFrom(wd, renderDir)
			}
			var findings []template.Finding
			for _, name := range names {
				files, found := template.Lint(fsys, name)
				findings = append(findings, found...)
				if renderDir != "" && len(found) == 0 {
					if err := writeRender(filepath.Join(renderDir, name), files); err != nil {
						return err
					}
				}
			}
			if err := printFindings(f, format, findings); err != nil {
				return err
			}
			if len(findings) > 0 {
				return exitcode.New(exitcode.Unexpected, "%s in %s", plural(len(findings), "problem"), plural(len(names), "template"))
			}
			f.infof("%s checked, no problems.", plural(len(names), "template"))
			return nil
		},
	}
	cmd.Flags().StringVar(&renderDir, "render-dir", "", "also write each clean template's sample render to DIR/<template>")
	_ = cmd.Flags().MarkHidden("render-dir")
	return cmd
}

func absFrom(wd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(wd, p)
}

// findTemplates returns dir itself when it holds a template.yaml, else its
// direct subdirectories that do, with an fs.FS rooted at their parent.
func findTemplates(dir string) (fs.FS, []string, error) {
	if _, err := os.Stat(filepath.Join(dir, template.FileName)); err == nil {
		return os.DirFS(filepath.Dir(dir)), []string{filepath.Base(dir)}, nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil, exitcode.Usagef("%s is not a directory", dir)
	}
	fsys := os.DirFS(dir)
	names, err := template.Discover(fsys)
	if err != nil {
		return nil, nil, err
	}
	if len(names) == 0 {
		return nil, nil, exitcode.Usagef("no %s in %s or its subdirectories", template.FileName, dir).
			WithHint("pass a template directory, or a directory of templates")
	}
	return fsys, names, nil
}

func writeRender(dir string, files []template.File) error {
	if _, err := os.Stat(dir); err == nil {
		return exitcode.New(exitcode.Conflict, "%s already exists", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, file := range files {
		p := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // a source tree for the user to build
			return err
		}
		if err := os.WriteFile(p, file.Data, file.Mode); err != nil { //nolint:gosec // modes come from the template: 0644 or 0755
			return err
		}
	}
	return nil
}

func printFindings(f *Factory, format output.Format, findings []template.Finding) error {
	if findings == nil {
		findings = []template.Finding{}
	}
	if format == output.Table {
		for _, fd := range findings {
			if _, err := fmt.Fprintln(f.IO.Out, fd.String()); err != nil {
				return err
			}
		}
		return nil
	}
	names := make([]string, 0, len(findings))
	for _, fd := range findings {
		n := fd.File
		if fd.Line > 0 {
			n = fmt.Sprintf("%s:%d", fd.File, fd.Line)
		}
		names = append(names, n)
	}
	return output.Print(f.IO.Out, format, output.View{
		Names: names,
		Data: struct {
			Findings []template.Finding `json:"findings"`
		}{findings},
	})
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
