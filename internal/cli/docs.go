package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/docs"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
)

func newDocsCmd(f *Factory) *cobra.Command {
	cmd := group("docs", "Read a service's docs", "doc")
	cmd.Long = `A service's docs are Markdown files under docs/ in its repo, with docs/index.md
as the start page. The Forge API reads them from the repo's default branch as
they are now: nothing is built or published. forge.yaml's docs: list sets the
order and titles of the pages.`
	cmd.AddCommand(newDocsListCmd(f), newDocsGetCmd(f), newDocsLintCmd(f))
	return cmd
}

func newDocsListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the services that have docs",
		Example: `  forge docs list
  forge docs list -o json | jq -r '.docSets[].service'`,
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
			resp, err := c.Docs.ListDocSets(ctx, connect.NewRequest(&forgev1.ListDocSetsRequest{}))
			if err != nil {
				return err
			}
			sets := resp.Msg.GetDocSets()
			if len(sets) == 0 && format == output.Table {
				f.infof("No service has docs yet. Add docs/index.md to a service's repo.")
				return nil
			}
			v := output.View{Headers: []string{"service", "team", "pages", "commit", "changed"}, Data: resp.Msg}
			if len(sets) == 0 {
				v.Data = map[string]any{"docSets": []any{}} // protojson omits empty lists
			}
			for _, ds := range sets {
				changed := "-"
				if ds.GetChangeTime() != nil {
					changed = age(f.Now().Sub(ds.GetChangeTime().AsTime())) + " ago"
				}
				v.Rows = append(v.Rows, []string{
					strings.TrimPrefix(ds.GetService(), "services/"), ds.GetOwnerTeam(),
					fmt.Sprint(ds.GetPageCount()), short(ds.GetCommit()), changed,
				})
				v.Names = append(v.Names, ds.GetService())
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
}

func newDocsGetCmd(f *Factory) *cobra.Command {
	var pages bool
	cmd := &cobra.Command{
		Use:   "get [service] [page]",
		Short: "Print a page of a service's docs",
		Long: `Print a page's Markdown: the start page (index.md) unless a page is named by
its path under docs/. Inside a service repo the service defaults to
forge.yaml's. With --pages, list the pages in the order of the side menu
instead. -o json prints the page with its title, commit and edit link.`,
		Example: `  forge docs get payments
  forge docs get payments runbook.md
  forge docs get payments --pages
  forge docs get payments api/errors.md -o json | jq -r .editUrl`,
		Args:              cobra.MaximumNArgs(2),
		ValidArgsFunction: f.completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			name, err := f.serviceName(args[:min(len(args), 1)])
			if err != nil {
				return err
			}
			page := ""
			if len(args) == 2 {
				page = strings.TrimPrefix(args[1], docs.Dir+"/")
			}
			if pages && page != "" {
				return exitcode.Usagef("--pages lists every page; do not name one")
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			c, err := f.Client(ctx, true)
			if err != nil {
				return err
			}
			if pages {
				resp, err := c.Docs.GetDocSet(ctx, connect.NewRequest(&forgev1.GetDocSetRequest{Service: name}))
				if err != nil {
					return err
				}
				v := output.View{Headers: []string{"page", "title"}, Data: resp.Msg.GetDocSet()}
				for _, p := range resp.Msg.GetDocSet().GetPages() {
					v.Rows = append(v.Rows, []string{p.GetPath(), p.GetTitle()})
					v.Names = append(v.Names, p.GetPath())
				}
				return output.Print(f.IO.Out, format, v)
			}
			resp, err := c.Docs.GetDocPage(ctx, connect.NewRequest(&forgev1.GetDocPageRequest{Service: name, Path: page}))
			if err != nil {
				return err
			}
			p := resp.Msg.GetPage()
			if format == output.Table {
				md := p.GetMarkdown()
				if !strings.HasSuffix(md, "\n") {
					md += "\n"
				}
				_, err := fmt.Fprint(f.IO.Out, md)
				return err
			}
			return output.Print(f.IO.Out, format, output.View{Data: p, Names: []string{p.GetPath()}})
		},
	}
	cmd.Flags().BoolVar(&pages, "pages", false, "list the pages instead of printing one")
	return cmd
}

func newDocsLintCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "lint [dir]",
		Short: "Check a repo's docs, without the API",
		Long: `Check the docs of the repo in dir (default: the current directory) as the
Forge API will read them: docs/index.md exists, the pages forge.yaml's docs:
list names exist, pages and images are within the limits, and links and
images between pages point at files that exist. It needs no API and no login.

Exits 0 when there are no problems and 1 when there are.`,
		Example: `  forge docs lint
  forge docs lint ../payments -o json | jq -r '.findings[].message'`,
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
			dir = absFrom(wd, dir)
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return exitcode.Usagef("%s is not a directory", dir)
			}
			var findings []docs.Finding
			var list []docs.Entry
			data, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
			if err == nil {
				var m *manifest.Manifest
				if m, err = manifest.Parse(data); err == nil {
					list = m.DocEntries()
				}
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				findings = append(findings, docs.Finding{File: manifest.FileName, Message: err.Error()})
			}
			findings = append(findings, docs.Lint(os.DirFS(dir), list)...)
			if err := printDocFindings(f, format, findings); err != nil {
				return err
			}
			if len(findings) > 0 {
				return exitcode.New(exitcode.Unexpected, "%s in the docs", plural(len(findings), "problem"))
			}
			f.infof("Docs checked, no problems.")
			return nil
		},
	}
}

func printDocFindings(f *Factory, format output.Format, findings []docs.Finding) error {
	if findings == nil {
		findings = []docs.Finding{}
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
			Findings []docs.Finding `json:"findings"`
		}{findings},
	})
}
