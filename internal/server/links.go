package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
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
	self, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if err != nil {
		return nil, err
	}
	app := self.GetApplication()
	if app == "" {
		return nil, refuseLink(svc, "the service is not a part of an application; make it one with 'forge service update %s --application <name>'", svc)
	}
	out := map[string]string{}
	for _, l := range parts {
		p, res := manifest.SplitLink(l)
		part, err := d.linkedPart(ctx, svc, app, p)
		if err != nil {
			return nil, err
		}
		if res != "" {
			continue // a database: linkedDatabase
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
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// refuseLink is the error of a link of svc that cannot be made.
func refuseLink(svc, format string, args ...any) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run.links of %s%s: "+format, append([]any{servicePrefix, svc}, args...)...))
}

// linkedPart is the catalog entry of p, which svc links to: it must be a
// part of svc's application app and run something.
func (d *Deploys) linkedPart(ctx context.Context, svc, app, p string) (*forgev1.Service, error) {
	part, err := d.o.Catalog.get(ctx, servicePrefix+p)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, refuseLink(svc, "%s%s is not registered", servicePrefix, p)
	}
	if err != nil {
		return nil, err
	}
	if part.GetApplication() != app {
		return nil, refuseLink(svc, "%s%s is not a part of the application %s", servicePrefix, p, app)
	}
	if i := part.GetInfrastructure(); i != nil && !i.GetRuns() {
		return nil, refuseLink(svc, "%s%s is infrastructure code and runs nothing to link to", servicePrefix, p)
	}
	return part, nil
}

// linkSecretName is the Secret in a part's namespace that holds Forge's
// copy of the URL of the database the part links to.
const linkSecretName = "forge-linked-database" //nolint:gosec // the name of a Secret, not a credential

// linkedDatabase is what svc needs in env to use another part's database.
type linkedDatabase struct {
	// values is the database as svc's values name it.
	values deployDatabase
	// part owns the database; secret is its Secret in the part's namespace.
	part, secret string
}

// linkedDatabase finds the link of parts that names another part's
// database ("<part>/<resource>") and checks that svc may use it in env:
// both are parts of the same application, and the part shares that
// database where it runs in env now. nil when no link names a database.
func (d *Deploys) linkedDatabase(ctx context.Context, svc string, parts []string, env string) (*linkedDatabase, error) {
	for _, l := range parts {
		p, res := manifest.SplitLink(l)
		if res == "" {
			continue
		}
		self, err := d.o.Catalog.get(ctx, servicePrefix+svc)
		if err != nil {
			return nil, err
		}
		if self.GetApplication() == "" {
			return nil, refuseLink(svc, "the service is not a part of an application; make it one with 'forge service update %s --application <name>'", svc)
		}
		if _, err := d.linkedPart(ctx, svc, self.GetApplication(), p); err != nil {
			return nil, err
		}
		owner := d.currentValues(ctx, p, env)
		switch {
		case owner == nil:
			return nil, refuseLink(svc, "%s%s does not run in %s yet, so its database %s cannot be used there: deploy it first", servicePrefix, p, env, res)
		case owner.Database != nil && owner.Database.Resource == "":
			// Written by a Forge that did not record which resource it is.
			return nil, refuseLink(svc, "%s%s must be deployed again in %s before its database %s can be shared", servicePrefix, p, env, res)
		case owner.Database == nil || owner.Database.Resource != res:
			return nil, refuseLink(svc, "%s%s has no database %s in %s (apply its resources, then deploy it)", servicePrefix, p, res, env)
		case !owner.Database.Shared:
			return nil, refuseLink(svc, "%s%s does not share its database %s in %s: set 'share: true' on the resource in its forge.yaml and deploy it", servicePrefix, p, res, env)
		}
		return &linkedDatabase{
			values: deployDatabase{Secret: linkSecretName, From: l},
			part:   p, secret: owner.Database.Secret,
		}, nil
	}
	return nil, nil
}

// linkNames is run.links as the API shows it: "services/<part>", and
// "services/<part>/resources/<name>" for a part's shared database.
func linkNames(parts []string) []string {
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, len(parts))
	for i, l := range parts {
		p, res := manifest.SplitLink(l)
		if out[i] = servicePrefix + p; res != "" {
			out[i] += "/resources/" + res
		}
	}
	return out
}

// dropLinkedDatabase removes Forge's copy of another part's database URL
// from svc's namespace in env, after a deploy without such a link. It is
// tried at every such deploy, so a copy that could not be removed is
// removed by the next one; a failure is told only when hadLink says there
// was a copy to remove.
func (d *Deploys) dropLinkedDatabase(ctx context.Context, svc, env, hadLink string, emit func(string, string)) {
	err := d.o.Cluster.DeleteLinkedDatabase(ctx, svc, env)
	switch {
	case hadLink == "":
	case err != nil:
		emit("database", fmt.Sprintf("the copy of %s's credentials in Secret %s could not be removed (%v); the next deploy tries again", hadLink, linkSecretName, err))
	default:
		emit("database", "removed the copy of "+hadLink+"'s credentials")
	}
}

// revokeSharedDatabase takes the copies of svc's database URL away from the
// parts of its application that linked to it in env, after a deploy that
// stopped sharing it. Their pods lose the database when they next start.
func (d *Deploys) revokeSharedDatabase(ctx context.Context, svc, env string, emit func(string, string)) {
	failed := func(err error) {
		emit("database", fmt.Sprintf("the database is no longer shared, but the copies of its credentials could not all be removed (%v): remove run.links from the parts that linked to it and deploy them", err))
	}
	self, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if err != nil {
		failed(err)
		return
	}
	if self.GetApplication() == "" {
		return
	}
	apps, err := d.o.Catalog.applications(ctx, self.GetApplication())
	if err != nil {
		failed(err)
		return
	}
	for _, app := range apps {
		for _, part := range app.GetParts() {
			p := strings.TrimPrefix(part.GetService(), servicePrefix)
			cur := d.currentValues(ctx, p, env)
			if p == svc || cur == nil || cur.Database == nil || !strings.HasPrefix(cur.Database.From, svc+"/") {
				continue
			}
			if err := d.o.Cluster.DeleteLinkedDatabase(ctx, p, env); err != nil {
				failed(err)
				continue
			}
			emit("database", fmt.Sprintf("no longer shared: removed the copy of its credentials from %s%s, whose pods lose the database when they next start", servicePrefix, p))
		}
	}
}

// linksHold says why svc cannot change application now: what it runs
// links to parts of the one it has, or shares its database with them. ""
// when nothing does.
func (d *Deploys) linksHold(ctx context.Context, svc string) string {
	for _, env := range deployEnvs {
		cur := d.currentValues(ctx, svc, env)
		switch {
		case cur == nil:
		case len(cur.Forge.Links) > 0:
			return fmt.Sprintf("what it runs in %s links to parts of the application (run.links: %s): remove the links from its forge.yaml and deploy it first", env, strings.Join(cur.Forge.Links, ", "))
		case cur.Database != nil && cur.Database.Shared:
			return fmt.Sprintf("it shares its database %s with the application in %s: remove 'share: true' from its forge.yaml and deploy it first", cur.Database.Resource, env)
		}
	}
	return ""
}
