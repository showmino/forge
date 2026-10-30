package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/pkg/client"
)

func newServiceConnectCmd(f *Factory) *cobra.Command {
	var name, team, health string
	var owners []string
	var port int
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "connect <repo> --team <team>",
		Short: "Connect an existing repo: Forge builds and deploys it",
		Long: `Connect an existing repo of the organization (github.com/<org>/<repo>) to
Forge. Forge registers the service and opens a pull request in the repo that
adds forge.yaml and Forge's workflow, which builds the image and deploys main
to staging. Merging the pull request is the first deploy; production is a
promotion ('forge deploy promote <name>').

The repo needs a Dockerfile at its root. Without one, Forge proposes one for
Go (go.mod), Node (package.json) or Python (requirements.txt or
pyproject.toml) in the same pull request; check it before merging. The image
runs as a non-root user with a read-only filesystem (only /tmp is writable),
and the app listens on $PORT.

The owning team must be able to write to the repo (a Forge admin may connect
any repo of the organization); Forge changes no one's access to it. A repo
that has a forge.yaml already is registered with 'forge service register'.
<name> defaults to the repo's name.`,
		Example: `  forge service connect github.com/my-org/tool --team web
  forge service connect github.com/my-org/legacy-api --name billing-api --team billing --port 9090 --health /status
  forge service connect github.com/my-org/tool --team web --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return exitcode.Usagef("name the repo: github.com/<org>/<repo>")
			}
			repo := strings.TrimSuffix(strings.TrimSuffix(args[0], "/"), ".git")
			if name == "" {
				name = strings.ToLower(repo[strings.LastIndex(repo, "/")+1:])
			}
			if !manifest.ValidID(name) {
				return exitcode.Usagef("%q is not a valid service name (lowercase letters, digits and '-'): name it with --name", name)
			}
			if team == "" {
				return exitcode.Usagef("--team is required")
			}
			if port < 0 || port > 65535 {
				return exitcode.Usagef("--port %d must be 1-65535", port)
			}
			if health != "" && !strings.HasPrefix(health, "/") {
				return exitcode.Usagef("--health %q must be a path such as /healthz", health)
			}
			format, err := f.Format()
			if err != nil {
				return err
			}
			req := &forgev1.CreateServiceRequest{
				ServiceId: name, RepoUrl: repo, OwnerTeam: team, Owners: owners,
				RequestId: client.NewRequestID(), ValidateOnly: dryRun,
			}
			if port != 0 || health != "" {
				req.RunOptions = &forgev1.RunOptions{Port: int32(port), Health: health} //nolint:gosec // checked above
			}
			ctx, cancel := f.withTimeout(cmd.Context())
			defer cancel()
			sc, err := f.StreamClient(ctx)
			if err != nil {
				return err
			}
			final, err := f.streamCreate(ctx, sc, req)
			if errors.Is(err, errMayStillFinish) {
				return exitcode.From(errors.Unwrap(err)).WithHint(fmt.Sprintf("the connect may still finish on the Forge API; check 'forge service get %s'", name))
			}
			if err != nil {
				return err
			}
			switch {
			case final.GetPlan() != nil:
				p := final.GetPlan()
				if format != output.Table {
					return output.Print(f.IO.Out, format, output.View{Names: p.GetFiles(), Data: p})
				}
				var b strings.Builder
				fmt.Fprintf(&b, "Would connect %s as services/%s, with a pull request that adds:\n", strings.TrimPrefix(p.GetRepoUrl(), "https://"), name)
				for _, path := range p.GetFiles() {
					if path == "Dockerfile" && p.GetDockerfile() != "" {
						path += " (proposed for " + p.GetDockerfile() + ")"
					}
					fmt.Fprintf(&b, "  %s\n", path)
				}
				_, err := fmt.Fprint(f.IO.Out, b.String())
				return err
			case final.GetChange() != nil:
				ch := final.GetChange()
				if format != output.Table {
					return output.Print(f.IO.Out, format, output.View{Names: []string{ch.GetPullRequestUrl()}, Data: ch})
				}
				msg := fmt.Sprintf("%s connected\n%s\nMerge it: its workflow builds the image and deploys main to staging.\n", ch.GetService().GetName(), ch.GetPullRequestUrl())
				if ch.GetDockerfile() != "" {
					msg += fmt.Sprintf("The repo had no Dockerfile: the pull request proposes one for %s; check it first.\n", ch.GetDockerfile())
				}
				if ch.GetNeedsRegistrySecrets() {
					msg += "Add the repo secrets FORGE_REGISTRY_USERNAME and FORGE_REGISTRY_PASSWORD before merging: the workflow logs in to the registry with them.\n"
				}
				_, err := fmt.Fprint(f.IO.Out, msg)
				return err
			case final.GetFailure() != nil:
				fl := final.GetFailure()
				hint := "the service was removed from the catalog again; fix the cause and run the same command"
				if !fl.GetRolledBack() {
					hint = fmt.Sprintf("the service is still in the catalog; delete it with 'forge service delete %s' before trying again", name)
				}
				return exitcode.New(exitcode.OperationFailed, "%s failed: %s", fl.GetStep(), fl.GetMessage()).WithHint(hint)
			}
			return errors.New("the Forge API ended the connect without a result")
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&name, "name", "", "the service's name (default: the repo's name)")
	fl.StringVar(&team, "team", "", "owning team (it must be able to write to the repo)")
	fl.StringSliceVar(&owners, "owner", nil, "owner email or handle (repeatable)")
	fl.IntVar(&port, "port", 0, "the port the app listens on (default 8080)")
	fl.StringVar(&health, "health", "", "the path its readiness is probed at (default /healthz)")
	fl.BoolVar(&dryRun, "dry-run", false, "check the repo and list the files; change nothing")
	return cmd
}
