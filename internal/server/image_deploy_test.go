package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/registry"
)

const (
	nginx127 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	nginx128 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// newImageFixture is the deploy fixture with nginx 1.27 and 1.28 in the
// registry and digests looked up there; tweak may change the options more.
func newImageFixture(t *testing.T, tweak func(*DeployOptions)) *deployFixture {
	t.Helper()
	var images *envtest.Images
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		images = o.Images.(*envtest.Images)
		o.Digests = images
		if tweak != nil {
			tweak(o)
		}
	})
	images.AddDigest("docker.io/library/nginx:1.27", nginx127)
	images.AddDigest("docker.io/library/nginx:1.28", nginx128)
	images.AddDigest("docker.io/library/nginx:latest", nginx128)
	return f
}

// create runs CreateService as the writer and returns its messages.
func (f *deployFixture) create(t *testing.T, req *forgev1.CreateServiceRequest) ([]*forgev1.CreateServiceResponse, error) {
	t.Helper()
	stream, err := f.catalogClient.CreateService(context.Background(), as("writer", req))
	if err != nil {
		return nil, err
	}
	var out []*forgev1.CreateServiceResponse
	for stream.Receive() {
		out = append(out, stream.Msg())
	}
	return out, stream.Err()
}

func doneSteps(msgs []*forgev1.CreateServiceResponse) string {
	var out []string
	for _, m := range msgs {
		if s := m.GetStep(); s != nil && s.GetState() == forgev1.CreateStep_STATE_DONE {
			out = append(out, s.GetName())
		}
	}
	return strings.Join(out, ",")
}

func (f *deployFixture) deployImage(t *testing.T, svc, env, ref string) *forgev1.Operation {
	t.Helper()
	resp, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/" + svc, Env: env, Ref: ref}))
	if err != nil {
		t.Fatalf("deploy %s %s: %v", env, ref, err)
	}
	op := wait(t, f.ops, resp.Msg.GetOperation().GetName())
	if op.GetError() != nil {
		t.Fatalf("deploy %s %s: %v", env, ref, op.GetError())
	}
	return op
}

func TestCreateFromImageThenDeployAndPromote(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, nil)
	ctx := context.Background()
	msgs, err := f.create(t, &forgev1.CreateServiceRequest{
		ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing",
		RunOptions: &forgev1.RunOptions{Port: 80, Health: "/", Env: map[string]string{"MODE": "demo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := doneSteps(msgs); got != "check_image,write_manifest,register" {
		t.Errorf("steps %s", got)
	}
	if svc := msgs[len(msgs)-1].GetService(); svc.GetName() != "services/web" || svc.GetRepoUrl() != "" || svc.GetOwnerTeam() != "billing" {
		t.Fatalf("final message: %v", msgs[len(msgs)-1])
	}
	data, err := f.repo.Read(ctx, "services/web/forge.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(data)
	if err != nil || m.RunImage() != "docker.io/library/nginx:1.27" || m.RunPort() != 80 || m.RunHealth() != "/" || m.Run.Env["MODE"] != "demo" {
		t.Fatalf("forge.yaml %v:\n%s", err, data)
	}

	// A deploy pins the tag to its digest and runs as the tag.
	op := f.deployImage(t, "web", "staging", "")
	if got := eventSteps(op); got != "resolve,image,release,commit,namespace,sync" {
		t.Errorf("steps %s", got)
	}
	v, _, err := f.deploys.current(ctx, "web", "staging")
	if err != nil || v.Image != "docker.io/library/nginx@"+nginx127 || v.Port != 80 || v.Forge.Commit != nginx127 || v.Forge.Version != "1.27" || v.Vars["MODE"] != "demo" {
		t.Errorf("staging values: %+v %v", v, err)
	}

	// A new tag: deployed, and written into forge.yaml for the next deploy.
	f.deployImage(t, "web", "staging", "1.28")
	if v, _, _ := f.deploys.current(ctx, "web", "staging"); v.Image != "docker.io/library/nginx@"+nginx128 || v.Forge.Version != "1.28" {
		t.Errorf("1.28 values: %+v", v)
	}
	data, _ = f.repo.Read(ctx, "services/web/forge.yaml", "")
	if m, err := manifest.Parse(data); err != nil || m.RunImage() != "docker.io/library/nginx:1.28" || m.Run.Env["MODE"] != "demo" {
		t.Errorf("forge.yaml after 1.28: %v\n%s", err, data)
	}

	// Production gets the very digest staging ran, as the same version.
	p, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/web"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, p.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("promote: %v", op.GetError())
	}
	if v, _, _ := f.deploys.current(ctx, "web", "production"); v.Image != "docker.io/library/nginx@"+nginx128 || v.Forge.Version != "1.28" || v.Forge.Kind != kindPromote {
		t.Errorf("production values: %+v", v)
	}
}

func TestImageRulesAndLookups(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, func(o *DeployOptions) { o.AllowedRegistries = []string{"docker.io"} })
	ctx := context.Background()
	for _, tc := range []struct {
		image, want string
	}{
		{"nginx:latest", "the tag latest moves"},
		{"nginx", "the tag latest moves"},
		{"nginx:0.0", "is not in its registry"},
		{"ghcr.io/acme/tool:2.3", "images may only come from docker.io"},
	} {
		_, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: tc.image, OwnerTeam: "billing"})
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("create %s: %v, want %q", tc.image, err, tc.want)
		}
	}
	if _, err := f.repo.Read(ctx, "services/web/forge.yaml", ""); err == nil {
		t.Error("a refused create wrote forge.yaml")
	}
	if has, _ := f.catalog.has(ctx, "services/web"); has {
		t.Error("a refused create registered the service")
	}

	// validate_only checks, and writes nothing.
	msgs, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing", ValidateOnly: true})
	if err != nil || len(msgs) == 0 || strings.Join(msgs[len(msgs)-1].GetPlan().GetFiles(), ",") != "services/web/forge.yaml" {
		t.Fatalf("validate_only: %v %v", msgs, err)
	}
	if _, err := f.repo.Read(ctx, "services/web/forge.yaml", ""); err == nil {
		t.Error("validate_only wrote forge.yaml")
	}

	// Template or image, and what is a template's stays the template's.
	for _, req := range []*forgev1.CreateServiceRequest{
		{ServiceId: "web", Image: "nginx:1.27", Template: "templates/go-grpc", OwnerTeam: "billing"},
		{ServiceId: "web", Image: "nginx:1.27", Parameters: map[string]string{"a": "b"}, OwnerTeam: "billing"},
	} {
		if _, err := f.create(t, req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("create %v: %v, want InvalidArgument", req, err)
		}
	}

	// A deploy of an existing service checks the tag the same way.
	if _, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing"}); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{"latest": "the tag latest moves", "0.0": "is not in its registry", "-x": "tag"} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/web", Env: "staging", Ref: ref}))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("deploy %s: %v, want %q", ref, err, want)
		}
	}
}

// Every service runs as a non-root user, so an image that would run as root
// (no USER, 0, root) or as a user Kubernetes cannot check (a name) is
// refused before anything is written or deployed.
func TestImageMustRunAsNonRoot(t *testing.T) {
	t.Parallel()
	var images *envtest.Images
	f := newImageFixture(t, func(o *DeployOptions) { images = o.Images.(*envtest.Images) })
	ctx := context.Background()
	digest := func(n int) string { return fmt.Sprintf("sha256:%064x", n+100) }
	for i, tc := range []struct{ user, want string }{
		{"", "runs as root"},
		{"0", "runs as root"},
		{"root:root", "runs as root"},
		{"0:101", "runs as root"},
		{"nginx", `runs as the user "nginx"`},
		{"101", ""},
		{"101:101", ""},
	} {
		image := fmt.Sprintf("docker.io/acme/tool:%d", i)
		images.AddDigest(image, digest(i+3))
		images.SetUser(digest(i+3), tc.user)
		name := fmt.Sprintf("tool-%d", i)
		_, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: name, Image: image, OwnerTeam: "billing"})
		if tc.want == "" {
			if err != nil {
				t.Errorf("user %q: %v", tc.user, err)
			}
			continue
		}
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "non-root") {
			t.Errorf("user %q: %v, want %q", tc.user, err, tc.want)
		}
		if _, err := f.repo.Read(ctx, "services/"+name+"/forge.yaml", ""); err == nil {
			t.Errorf("user %q: a refused create wrote forge.yaml", tc.user)
		}
	}

	// A new tag is checked too: tool-5 runs as 101, its tag 9 as root.
	images.AddDigest("docker.io/acme/tool:9", digest(9))
	images.SetUser(digest(9), "")
	_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/tool-5", Env: "staging", Ref: "9"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "runs as root") {
		t.Errorf("deploy a root tag: %v", err)
	}
}

// A service without a repo has no branch to preview, and a repo's own
// forge.yaml may not name an image.
func TestImageServiceBoundaries(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, nil)
	ctx := context.Background()
	if _, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing"}); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSource(ctx, f.catalog, f.deploys.o.GitHub, "showmino", "", "services/web", "")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "has no repo") {
		t.Errorf("preview of web: %v", err)
	}
	// A registered service with no repo and no forge.yaml in the deploy repo.
	if _, err := f.catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "bare", Service: &forgev1.Service{OwnerTeam: "billing"},
	})); err != nil {
		t.Fatal(err)
	}
	_, err = f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/bare", Env: "staging"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "forge create --image") {
		t.Errorf("deploy bare: %v", err)
	}

	g := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		gh := o.GitHub.(*envtest.GitHub)
		gh.Files[envSHA+":forge.yaml"] = []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  image: nginx:1.27\n")
	})
	if _, err := g.start(t, "staging"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "run.image") {
		t.Errorf("repo service with run.image: %v", err)
	}
}

func TestDeleteImageServiceRemovesItsForgeYAML(t *testing.T) {
	t.Parallel()
	f := &teardownFixture{deployFixture: newImageFixture(t, nil)}
	f.catalog.EnableTeardown(f.deploys, nil, f.ops)
	ctx := context.Background()
	if _, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing"}); err != nil {
		t.Fatal(err)
	}
	f.deployImage(t, "web", "staging", "")
	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.removes, ","); got != "deployments/web/staging" {
		t.Errorf("removes %s", got)
	}
	if op := wait(t, f.ops, op.GetName()); op.GetError() != nil {
		t.Fatalf("delete: %v", op.GetError())
	}
	if _, err := f.repo.Read(ctx, "services/web/forge.yaml", ""); err == nil {
		t.Error("forge.yaml left in the deploy repo")
	}
	if has, _ := f.catalog.has(ctx, "services/web"); has {
		t.Error("web left in the catalog")
	}

	// Created and never deployed: the delete still removes forge.yaml.
	if _, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "idle", Image: "nginx:1.27", OwnerTeam: "billing"}); err != nil {
		t.Fatal(err)
	}
	op, err = f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/idle"})
	if err != nil || op == nil {
		t.Fatalf("delete idle: %v %v", op, err)
	}
	if op := wait(t, f.ops, op.GetName()); op.GetError() != nil {
		t.Fatalf("delete idle: %v", op.GetError())
	}
	if _, err := f.repo.Read(ctx, "services/idle/forge.yaml", ""); err == nil {
		t.Error("idle's forge.yaml left in the deploy repo")
	}
}

// The images the services' own CI pushes are deployed from their repos, and
// a registry inside the network only when the platform names it.
func TestImageRulesGuardThePlatform(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, nil)
	images := f.deploys.o.Images.(*envtest.Images)
	images.AddDigest("ghcr.io/showmino/payments:1a2b3c4", nginx127)
	images.AddDigest("localhost:5000/tool:1", nginx127)
	for image, want := range map[string]string{
		"ghcr.io/showmino/payments:1a2b3c4": "is built by a service's CI, under ghcr.io/showmino/",
		"localhost:5000/tool:1":             "is an address or a local name",
	} {
		_, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: image, OwnerTeam: "billing"})
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
			t.Errorf("create %s: %v, want %q", image, err, want)
		}
	}
}

// A new tag becomes forge.yaml's only once it deployed; a rollback runs the
// earlier digest.
func TestImageServiceRetagAndRollback(t *testing.T) {
	t.Parallel()
	var argo *envtest.Argo
	// A deploy that does not come up fails at once.
	f := newImageFixture(t, func(o *DeployOptions) { o.SyncTimeout = 300 * time.Millisecond })
	argo = f.argo
	ctx := context.Background()
	if _, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing"}); err != nil {
		t.Fatal(err)
	}
	f.deployImage(t, "web", "staging", "")
	tagged := func() string {
		data, _ := f.repo.Read(ctx, "services/web/forge.yaml", "")
		m, err := manifest.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		return m.RunImage()
	}

	// 1.28 does not come up: forge.yaml keeps 1.27.
	argo.SetUnhealthy("web-staging", "Degraded")
	resp, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/web", Env: "staging", Ref: "1.28"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.Msg.GetOperation().GetName()); op.GetError() == nil {
		t.Fatal("an unhealthy deploy succeeded")
	}
	if got := tagged(); got != "docker.io/library/nginx:1.27" {
		t.Errorf("after a failed 1.28: forge.yaml names %s", got)
	}
	argo.SetUnhealthy("web-staging", "")

	f.deployImage(t, "web", "staging", "1.28")
	if got := tagged(); got != "docker.io/library/nginx:1.28" {
		t.Errorf("after 1.28: forge.yaml names %s", got)
	}
	// Back to the first deploy, 1.27 (the failed 1.28 is in the history too).
	recs, err := f.deploys.history(ctx, "web", "staging", 50)
	if err != nil || len(recs) != 3 {
		t.Fatalf("history: %v %v", recs, err)
	}
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/web/staging", To: recs[2].DeployCommit}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("rollback: %v", op.GetError())
	}
	if v, _, _ := f.deploys.current(ctx, "web", "staging"); v.Image != "docker.io/library/nginx@"+nginx127 || v.Forge.Kind != kindRollback {
		t.Errorf("after the rollback: %+v", v)
	}
}

// A CI token belongs to a repo: it cannot deploy a service without one.
func TestImageServiceRefusesCI(t *testing.T) {
	t.Parallel()
	f := newDeployFixtureAuth(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		images := o.Images.(*envtest.Images)
		images.AddDigest("docker.io/library/nginx:1.27", nginx127)
		o.Digests = images
	}, func(_ *Catalog, a Authenticator) Authenticator {
		static := a.(StaticAuthenticator)
		static["ci-web"] = Principal{
			Subject: "github:showmino/web", Permissions: []string{PermDeploy("web", "staging")},
			Workload: &Workload{Repository: "showmino/web", Service: "web", SHA: envSHA},
		}
		return static
	})
	if _, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.client.StartDeploy(context.Background(), as("ci-web", &forgev1.StartDeployRequest{Service: "services/web", Env: "staging"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "has no repo") {
		t.Errorf("CI deploy of web: %v", err)
	}
}

// A failed create takes back its own forge.yaml, never one written since;
// and a name is created once at a time.
func TestImageCreateRollbackAndClaim(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, nil)
	ctx := context.Background()
	quiet := func(string, string) {}
	mine := []byte("apiVersion: forge.dev/v1\nservice:\n  name: web\n  team: a\nrun:\n  image: nginx:1.27\n")
	theirs := []byte("apiVersion: forge.dev/v1\nservice:\n  name: web\n  team: b\nrun:\n  image: nginx:1.28\n")
	if err := f.deploys.writeManifest(ctx, "web", theirs, "theirs", quiet); err != nil {
		t.Fatal(err)
	}
	if err := f.deploys.removeManifestIf(ctx, "web", mine, quiet); err != nil {
		t.Fatal(err)
	}
	if data, err := f.repo.Read(ctx, "services/web/forge.yaml", ""); err != nil || string(data) != string(theirs) {
		t.Errorf("someone else's forge.yaml was removed: %v %q", err, data)
	}
	if err := f.deploys.removeManifestIf(ctx, "web", theirs, quiet); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Read(ctx, "services/web/forge.yaml", ""); err == nil {
		t.Error("the create's own forge.yaml was kept")
	}

	c := &creator{}
	release, err := c.claimName("web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.claimName("web"); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("second claim: %v, want Aborted", err)
	}
	release()
	if r, err := c.claimName("web"); err != nil {
		t.Errorf("claim after release: %v", err)
	} else {
		r()
	}
}

// brokenImages has every image, at one digest, but cannot say who it runs as.
type brokenImages struct{ err error }

func (brokenImages) Digest(context.Context, string) (string, error) { return nginx127, nil }
func (b brokenImages) User(context.Context, string) (string, error) { return "", b.err }

// A problem with the image itself is the image's (FailedPrecondition); one
// reaching the registry may pass (Unavailable).
func TestImageUserLookupErrors(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]connect.Code{
		fmt.Errorf("docker.io/acme/tool:1: the index names no linux image: %w", registry.ErrBadImage): connect.CodeFailedPrecondition,
		errors.New("GET /v2/acme/tool/blobs/x: 503 Service Unavailable"):                              connect.CodeUnavailable,
	} {
		d := &Deploys{o: DeployOptions{Digests: brokenImages{err}}}
		if _, got := d.pinImage(context.Background(), "tool", "acme/tool:1"); connect.CodeOf(got) != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
}
