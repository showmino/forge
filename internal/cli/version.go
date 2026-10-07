package cli

import (
	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/version"
)

type versionInfo struct {
	Client version.Info `json:"client"`
	Server *serverInfo  `json:"server,omitempty"`
}

type serverInfo struct {
	Version          string `json:"version"`
	MinClientVersion string `json:"minClientVersion,omitempty"`
	Context          string `json:"context,omitempty"`
	Error            string `json:"error,omitempty"`
}

func newVersionCmd(f *Factory) *cobra.Command {
	var clientOnly bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and the server version of the current context",
		Example: `  forge version
  forge version --client -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := f.Format()
			if err != nil {
				return err
			}
			info := versionInfo{Client: version.Get()}
			info.Client.Version = f.Version
			if !clientOnly {
				info.Server = f.serverVersion(cmd)
			}
			v := output.View{
				Headers: []string{"component", "version", "detail"},
				Rows:    [][]string{{"client", info.Client.Version, short(info.Client.Commit) + " " + info.Client.Date}},
				Data:    info,
			}
			if s := info.Server; s != nil {
				detail := s.Context
				if s.Error != "" {
					detail = s.Error
				}
				v.Rows = append(v.Rows, []string{"server", s.Version, detail})
			}
			return output.Print(f.IO.Out, format, v)
		},
	}
	cmd.Flags().BoolVar(&clientOnly, "client", false, "print only the client version; do not contact the server")
	return cmd
}

// serverVersion asks the current context's API for its version. Failure is
// reported in the output, not as an error: 'forge version' must always work.
func (f *Factory) serverVersion(cmd *cobra.Command) *serverInfo {
	r, err := f.Resolved()
	if err != nil || r.APIURL == "" {
		return nil
	}
	ctx, cancel := f.withTimeout(cmd.Context())
	defer cancel()
	c, err := f.Client(ctx, false)
	if err != nil {
		return &serverInfo{Context: r.ContextName, Error: err.Error()}
	}
	resp, err := c.Auth.GetServerInfo(ctx, connect.NewRequest(&forgev1.GetServerInfoRequest{}))
	if err != nil {
		return &serverInfo{Version: "unknown", Context: r.ContextName, Error: err.Error()}
	}
	return &serverInfo{Version: resp.Msg.GetServerVersion(), MinClientVersion: resp.Msg.GetMinClientVersion(), Context: r.ContextName}
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
