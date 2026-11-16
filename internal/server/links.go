package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// links works out run.links for svc in env: for each part it names, the
// variable its address goes in and the address, which is the part's
// Service inside the cluster in the same environment. A link is refused
// unless svc and the part are parts of the same application. Whether the
// part runs in env is not checked: a part must tolerate a neighbour that is
// not there yet.
func (d *Deploys) links(ctx context.Context, svc string, parts []string, env string) (map[string]string, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	refuse := func(format string, args ...any) error {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run.links of %s%s: "+format, append([]any{servicePrefix, svc}, args...)...))
	}
	self, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if err != nil {
		return nil, err
	}
	app := self.GetApplication()
	if app == "" {
		return nil, refuse("the service is not a part of an application; make it one with 'forge service update %s --application <name>'", svc)
	}
	out := make(map[string]string, len(parts))
	for _, p := range parts {
		part, err := d.o.Catalog.get(ctx, servicePrefix+p)
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, refuse("%s%s is not registered", servicePrefix, p)
		}
		if err != nil {
			return nil, err
		}
		if part.GetApplication() != app {
			return nil, refuse("%s%s is not a part of the application %s", servicePrefix, p, app)
		}
		if i := part.GetInfrastructure(); i != nil && !i.GetRuns() {
			return nil, refuse("%s%s is infrastructure code and runs nothing to link to", servicePrefix, p)
		}
		// A part's Service is in the namespace <part>-<env>: Forge's own on
		// port 80, or the one a chart exposes.
		name, port := p, int32(80)
		if part.GetRepoUrl() == "" {
			chart, err := d.manifestChart(ctx, p)
			if err != nil && !errors.Is(err, github.ErrNotFound) {
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the forge.yaml of %s%s: %w", servicePrefix, p, err))
			}
			if chart != nil {
				name, port = chart.Expose.Service, int32(chart.Expose.Port) //nolint:gosec // a port, checked by the manifest
			}
		}
		url := "http://" + name + "." + p + "-" + env + ".svc"
		if port != 80 {
			url += ":" + strconv.Itoa(int(port))
		}
		out[manifest.LinkEnv(p)] = url
	}
	return out, nil
}

// linkNames is run.links as the API shows it: service names.
func linkNames(parts []string) []string {
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = servicePrefix + p
	}
	return out
}
