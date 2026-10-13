package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/template"
)

func newTemplateCmd(f *Factory) *cobra.Command {
	cmd := group("template", "Inspect and lint service templates", "templates")
	cmd.Long = `Templates scaffold new services with 'forge create'. A template is a directory
with a template.yaml (typed parameters) and a skeleton/ rendered with Go
text/template. The platform team releases them by tagging the template repo.`
	cmd.AddCommand(newTemplateListCmd(f), newTemplateGetCmd(f), newTemplateLintCmd(f))
	return cmd
}

func newTemplateListCmd(f *Factory) *cobra.Command {
	var tag, version string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the templates of a release",
		Long:    "List the templates of a release of the template repo, the newest by default.",
		Example: `  forge template list
  forge template list --tag go
  forge template list --version v1.0.0 -o json | jq -r '.templates[].name'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Templates.ListTemplates(ctx, connect.NewRequest(&forgev1.ListTemplatesRequest{Version: version, Tag: tag}))
			if err != nil {
				return err
			}
			ts := resp.Msg.GetTemplates()
			if len(ts) == 0 && format == output.Table {
				f.infof("No templates found.")
				return nil
			}
			v := output.View{Headers: []string{"name", "version", "tags", "description"}, Data: resp.Msg}
			if len(ts) == 0 {
				// protojson omits empty lists; keep '.templates[]' working in jq.
				v.Data = map[string]any{"templates": []any{}}
			}
			for _, t := range ts {
				v.Rows = append(v.Rows, []string{strings.TrimPrefix(t.GetName(), "templates/"), t.GetVersion(), strings.Join(t.GetTags(), ", "), t.GetDescription()})
				v.Names = append(v.Names, t.GetName())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "only templates with this tag")
	cmd.Flags().StringVar(&version, "version", "", "release tag (default: the newest)")
	return cmd
}

func newTemplateGetCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>[@version]",
		Short: "Show a template and its parameters",
		Example: `  forge template get go-grpc
  forge template get go-grpc@v1.0.0 -o json | jq '.parameters'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, version, _ := strings.Cut(args[0], "@")
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			resp, err := c.Templates.GetTemplate(ctx, connect.NewRequest(&forgev1.GetTemplateRequest{
				Name:    "templates/" + strings.TrimPrefix(name, "templates/"),
				Version: version,
			}))
			if err != nil {
				return err
			}
			t := resp.Msg.GetTemplate()
			if format != output.Table {
				return output.Print(f.IO.Out, format, output.View{Names: []string{t.GetName()}, Data: t})
			}
			if err := output.Print(f.IO.Out, format, output.View{
				Headers: []string{"field", "value"},
				Rows: [][]string{
					{"name", t.GetName()},
					{"version", t.GetVersion()},
					{"title", t.GetTitle()},
					{"description", t.GetDescription()},
					{"tags", strings.Join(t.GetTags(), ", ")},
					{"owner", t.GetOwner()},
					{"run options", cmp.Or(strings.Join(t.GetOptions(), ", "), "none")},
				},
			}); err != nil {
				return err
			}
			if len(t.GetParameters()) == 0 {
				return nil
			}
			params := output.View{Headers: []string{"parameter", "type", "required", "default", "description"}}
			for _, p := range t.GetParameters() {
				required := "no"
				if p.GetRequired() {
					required = "yes"
				}
				params.Rows = append(params.Rows, []string{p.GetName(), paramType(p), required, p.GetDefaultValue(), p.GetDescription()})
			}
			if _, err := fmt.Fprintln(f.IO.Out); err != nil {
				return err
			}
			return output.Print(f.IO.Out, format, params)
		},
	}
}

// paramType reads "int", "string", or "enum (gold, silver)".
func paramType(p *forgev1.TemplateParameter) string {
	s := strings.ToLower(strings.TrimPrefix(p.GetType().String(), "TYPE_"))
	if p.GetType() == forgev1.TemplateParameter_TYPE_ENUM {
		s += " (" + strings.Join(p.GetValues(), ", ") + ")"
	}
	return s
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
