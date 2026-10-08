// Package cli builds the forge command tree. There are no package-level
// globals: every command gets its dependencies through Deps, so tests inject
// fakes and run in parallel.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/showmino/forge/internal/auth"
	"github.com/showmino/forge/internal/config"
	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/output"
	"github.com/showmino/forge/internal/version"
	"github.com/showmino/forge/pkg/client"
)

// IOStreams are the process's standard streams.
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
	// InTTY and OutTTY report whether stdin and stdout are terminals.
	InTTY, OutTTY bool
}

// Deps are everything a command needs from the outside world.
type Deps struct {
	IO IOStreams
	// Env looks up environment variables.
	Env config.Env
	// Store holds credentials. Defaults to the OS keyring with a file fallback.
	Store auth.Store
	// HTTPClient is used for the API and the identity provider.
	HTTPClient *http.Client
	// Getwd returns the working directory, for finding forge.yaml.
	Getwd func() (string, error)
	Now   func() time.Time
	// Version is the CLI's version, normally version.Get().Version.
	Version string
}

// DefaultDeps wires Deps to the real process.
func DefaultDeps(stdin io.Reader, stdout, stderr io.Writer, inTTY, outTTY bool) Deps {
	return Deps{
		IO:      IOStreams{In: stdin, Out: stdout, ErrOut: stderr, InTTY: inTTY, OutTTY: outTTY},
		Env:     os.LookupEnv,
		Getwd:   os.Getwd,
		Now:     time.Now,
		Version: version.Get().Version,
	}
}

// globalFlags are the persistent flags on the root command.
type globalFlags struct {
	context   string
	apiURL    string
	namespace string
	output    string
	quiet     bool
	verbose   bool
	noInput   bool
	noColor   bool
	timeout   time.Duration
}

// Factory lazily builds config, clients and printers for one invocation.
type Factory struct {
	Deps
	flags globalFlags

	once       sync.Once
	configPath string
	cfg        *config.Config
	resolved   config.Resolved
	manifest   *manifest.Manifest
	manifestAt string
	// manifestErr is set when a forge.yaml exists but is invalid; err holds it too.
	manifestErr error
	err         error

	skewOnce sync.Once
}

func newFactory(d Deps) *Factory {
	if d.Env == nil {
		d.Env = func(string) (string, bool) { return "", false }
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Getwd == nil {
		d.Getwd = os.Getwd
	}
	if d.Version == "" {
		d.Version = version.Get().Version
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Factory{Deps: d}
}

func (f *Factory) load() error {
	f.once.Do(func() {
		f.configPath, f.err = config.DefaultPath(f.Env)
		if f.err != nil {
			return
		}
		f.cfg, f.err = config.Load(f.configPath)
		if f.err != nil {
			return
		}
		var d config.Defaults
		d.Output = string(output.Table)
		if wd, err := f.Getwd(); err == nil {
			if p, err := manifest.Find(wd); err == nil && p != "" {
				m, err := manifest.Load(p)
				if err != nil {
					f.manifestErr = exitcode.Usagef("%v", err)
					f.err = f.manifestErr
					return
				}
				f.manifest, f.manifestAt = m, p
				d.Namespace = m.Namespace
			}
		}
		f.resolved, f.err = config.Resolve(f.cfg, f.Env, config.Overrides{
			Context: f.flags.context, APIURL: f.flags.apiURL, Namespace: f.flags.namespace, Output: f.flags.output,
		}, d)
		if f.err != nil {
			f.err = exitcode.Usagef("%v", f.err)
		}
	})
	return f.err
}

// Config returns the user config and its path. It succeeds when the config
// loaded, even if a later step (forge.yaml, resolving the context) failed.
func (f *Factory) Config() (*config.Config, string, error) {
	if err := f.load(); err != nil && f.cfg == nil {
		return nil, "", err
	}
	return f.cfg, f.configPath, nil
}

// Resolved returns the effective settings.
func (f *Factory) Resolved() (config.Resolved, error) {
	err := f.load()
	return f.resolved, err
}

// Manifest returns forge.yaml from the working directory or a parent, if any,
// and an error if one exists but is invalid.
func (f *Factory) Manifest() (*manifest.Manifest, string, error) {
	_ = f.load()
	return f.manifest, f.manifestAt, f.manifestErr
}

// Format returns the output format.
func (f *Factory) Format() (output.Format, error) {
	r, err := f.Resolved()
	if err != nil {
		return "", err
	}
	fm, err := output.Parse(r.Output)
	if err != nil {
		return "", exitcode.Usagef("%v", err)
	}
	return fm, nil
}

// CredentialStore returns the configured store, defaulting to keyring + file.
func (f *Factory) CredentialStore() auth.Store {
	if f.Store != nil {
		return f.Store
	}
	path, err := config.DefaultPath(f.Env)
	if err != nil {
		path = "credentials.json"
	}
	f.Store = &auth.FallbackStore{
		Primary:   auth.KeyringStore{},
		Secondary: &auth.FileStore{Path: filepath.Join(filepath.Dir(path), "credentials.json")},
	}
	return f.Store
}

// TokenSource returns FORGE_TOKEN if set, otherwise stored credentials.
func (f *Factory) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	r, err := f.Resolved()
	if err != nil {
		return nil, err
	}
	if r.Token != "" {
		return auth.StaticToken(r.Token), nil
	}
	return &loginHint{auth.TokenSource(ctx, f.CredentialStore(), r.CredentialKey(), f.HTTPClient, f.Now)}, nil
}

// loginHint turns ErrLoginRequired into exit code 4 with a hint.
type loginHint struct{ ts oauth2.TokenSource }

func (l *loginHint) Token() (*oauth2.Token, error) {
	t, err := l.ts.Token()
	if errors.Is(err, auth.ErrLoginRequired) {
		return nil, exitcode.New(exitcode.Unauthenticated, "%v", err).WithHint("run 'forge auth login'")
	}
	return t, err
}

// APIURL returns the API URL or a usage error naming how to set one.
func (f *Factory) APIURL() (string, error) {
	r, err := f.Resolved()
	if err != nil {
		return "", err
	}
	if r.APIURL == "" {
		return "", exitcode.Usagef("%v", config.ErrNoContext).
			WithHint("run 'forge context set <name> --api <url>' then 'forge context use <name>', or pass --api-url")
	}
	return r.APIURL, nil
}

// Client returns an API client. withAuth=false sends no credentials.
func (f *Factory) Client(ctx context.Context, withAuth bool) (*client.Client, error) {
	url, err := f.APIURL()
	if err != nil {
		return nil, err
	}
	o := client.Options{
		HTTPClient:    f.HTTPClient,
		ClientVersion: f.Version,
		OnResponse:    f.onResponse,
	}
	if withAuth {
		ts, err := f.TokenSource(ctx)
		if err != nil {
			return nil, err
		}
		o.TokenSource = ts
	}
	return client.New(url, o)
}

func (f *Factory) onResponse(procedure string, h http.Header) {
	if f.flags.verbose {
		fmt.Fprintf(f.IO.ErrOut, "%s request-id=%s server=%s\n", procedure, h.Get(client.HeaderRequestID), h.Get(client.HeaderServerVersion))
	}
	f.skewOnce.Do(func() {
		if msg := skewWarning(f.Version, h.Get(client.HeaderServerVersion), h.Get(client.HeaderMinClientVersion)); msg != "" && !f.flags.quiet {
			fmt.Fprintln(f.IO.ErrOut, "warning: "+msg)
		}
	})
}

// skewWarning implements the support window: the server's current minor
// version and the one before it.
func skewWarning(cli, server, minClient string) string {
	if minClient != "" && version.Compare(cli, minClient) < 0 {
		return fmt.Sprintf("forge %s is older than the oldest version this server accepts (%s); upgrade the CLI", cli, minClient)
	}
	cv, okC := majorMinor(cli)
	sv, okS := majorMinor(server)
	if !okC || !okS {
		return ""
	}
	if cv[0] != sv[0] || sv[1]-cv[1] > 1 {
		return fmt.Sprintf("forge %s is more than one minor version behind the server (%s); upgrade the CLI", cli, server)
	}
	return ""
}

func majorMinor(v string) ([2]int, bool) {
	var out [2]int
	var patch int
	if _, err := fmt.Sscanf(trimV(v), "%d.%d.%d", &out[0], &out[1], &patch); err != nil {
		return out, false
	}
	return out, true
}

func trimV(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		return v[1:]
	}
	return v
}

// withTimeout applies --timeout to ctx.
func (f *Factory) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.flags.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, f.flags.timeout)
}

// interactive reports whether prompting is allowed.
func (f *Factory) interactive() bool {
	return f.IO.InTTY && f.IO.OutTTY && !f.flags.noInput
}

// infof writes a diagnostic to stderr unless --quiet.
func (f *Factory) infof(format string, args ...any) {
	if !f.flags.quiet {
		fmt.Fprintf(f.IO.ErrOut, format+"\n", args...)
	}
}
