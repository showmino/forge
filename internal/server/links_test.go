package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

// run.links gives a part the address of another part of its application,
// in the environment it is deployed or promoted to.
func TestDeployLinks(t *testing.T) {
	t.Parallel()
	linked := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n  application: shop\nrun:\n  port: 9090\n  health: /ready\n  links: [ledger]\n"
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.GitHub.(*envtest.GitHub).Files[envSHA+":forge.yaml"] = []byte(linked)
	})
	ctx := writerCtx()
	join := func(svc, app string) {
		t.Helper()
		if _, err := f.catalog.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service:    &forgev1.Service{Name: "services/" + svc, Application: app},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"application"}},
		})); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(want string) {
		t.Helper()
		_, err := f.start(t, "staging")
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
			t.Fatalf("want a refusal with %q, got %v", want, err)
		}
	}

	// The catalog decides who is a part, not forge.yaml.
	refused("the service is not a part of an application")
	join("payments", "shop")
	refused("services/ledger is not registered")
	if _, err := f.catalog.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing"},
	})); err != nil {
		t.Fatal(err)
	}
	refused("services/ledger is not a part of the application shop")
	join("ledger", "shop")

	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := f.values(t, "staging").Links; len(got) != 1 || got["LEDGER_URL"] != "http://ledger.ledger-staging.svc" {
		t.Errorf("staging links: %v", got)
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := f.values(t, "production").Links; len(got) != 1 || got["LEDGER_URL"] != "http://ledger.ledger-production.svc" {
		t.Errorf("production links: %v", got)
	}
	dep, err := f.client.GetDeployment(context.Background(), as("writer", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := dep.Msg.GetDeployment().GetLinks(); len(got) != 1 || got[0] != "services/ledger" {
		t.Errorf("the deployment's links: %v", got)
	}

	// A worker has no address to link to.
	head, tree, err := f.repo.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := deployValues{Name: "ledger", Env: "staging", Worker: true, Forge: deployRecord{Service: "services/ledger", Env: "staging", Worker: true}}
	if _, err := f.repo.CommitFile(context.Background(), head, tree, valuesPath("ledger", "staging"), w.marshal(), "test"); err != nil {
		t.Fatal(err)
	}
	refused("services/ledger is a worker and has no address to link to")

	// A part that left the application cannot be linked to any more.
	join("ledger", "")
	refused("services/ledger is not a part of the application shop")
}

// A part uses another part's shared database: Forge copies the URL into
// the linking part's namespace, keeps it up to date, and removes it with
// the link.
func TestDeployLinkedDatabase(t *testing.T) {
	t.Parallel()
	const base = "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n  application: shop\nrun:\n  port: 9090\n  health: /ready\n"
	var gh *envtest.GitHub
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		gh = o.GitHub.(*envtest.GitHub)
		gh.Files[envSHA+":forge.yaml"] = []byte(base + "  links: [ledger/main]\n")
	})
	ctx := writerCtx()
	if _, err := f.catalog.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing", Application: "shop"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.catalog.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service:    &forgev1.Service{Name: "services/payments", Application: "shop"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"application"}},
	})); err != nil {
		t.Fatal(err)
	}
	refused := func(want string) {
		t.Helper()
		_, err := f.start(t, "staging")
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
			t.Fatalf("want a refusal with %q, got %v", want, err)
		}
	}
	// ledger's staging values, as its own deploy would have written them.
	ledger := func(db *deployDatabase) {
		t.Helper()
		v := deployValues{Name: "ledger", Env: "staging", Database: db, Forge: deployRecord{Service: "services/ledger", Env: "staging"}}
		head, tree, err := f.repo.Head(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.CommitFile(context.Background(), head, tree, valuesPath("ledger", "staging"), v.marshal(), "test"); err != nil {
			t.Fatal(err)
		}
	}
	kube := f.cluster.Kube.CoreV1()
	source := func(data map[string][]byte) {
		t.Helper()
		_ = kube.Secrets("ledger-staging").Delete(context.Background(), "main-app", metav1.DeleteOptions{})
		if _, err := kube.Secrets("ledger-staging").Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "main-app"}, Data: data}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	copied := func() *corev1.Secret {
		t.Helper()
		s, err := kube.Secrets("payments-staging").Get(context.Background(), linkSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	refused("services/ledger does not run in staging yet")
	ledger(nil)
	refused("services/ledger has no database main in staging")
	ledger(&deployDatabase{Secret: "main-app", Resource: "main"})
	refused("services/ledger does not share its database main in staging")
	ledger(&deployDatabase{Secret: "main-app", Resource: "main", Shared: true})

	// The fake Argo CD makes ledger's namespace from its values.
	deadline := time.Now().Add(5 * time.Second)
	for f.cluster.checkServiceNamespace(context.Background(), "ledger", "staging") != nil {
		if time.Now().After(deadline) {
			t.Fatal("ledger-staging did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	source(map[string][]byte{"uri": []byte("postgresql://app:one@main-rw:5432/app"), "fqdn-uri": []byte("postgresql://app:one@main-rw.ledger-staging.svc.cluster.local:5432/app"), "password": []byte("one")})

	op := f.deployRef(t, "staging", "")
	if op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if db := f.values(t, "staging").Database; db == nil || db.Secret != linkSecretName || db.From != "ledger/main" || db.Shared {
		t.Errorf("values: %+v", db)
	}
	// Only the URL crosses, by the name that resolves from another namespace.
	if s := copied(); len(s.Data) != 1 || string(s.Data["uri"]) != "postgresql://app:one@main-rw.ledger-staging.svc.cluster.local:5432/app" || s.Annotations[annotationCopiedFrom] != "ledger-staging/main-app" {
		t.Errorf("the copy: %d keys, from %q", len(s.Data), s.Annotations[annotationCopiedFrom])
	}
	// The operation's events name the database, never its URL.
	for _, e := range op.GetEvents() {
		if strings.Contains(e.GetMessage(), "postgresql://") {
			t.Errorf("an event carries the URL: %s", e.GetStep())
		}
	}

	// A changed password reaches the copy at the next deploy.
	source(map[string][]byte{"fqdn-uri": []byte("postgresql://app:two@main-rw.ledger-staging.svc.cluster.local:5432/app")})
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if s := copied(); string(s.Data["uri"]) != "postgresql://app:two@main-rw.ledger-staging.svc.cluster.local:5432/app" {
		t.Error("the copy was not brought up to date")
	}
	gone := func(why string) {
		t.Helper()
		if _, err := kube.Secrets("payments-staging").Get(context.Background(), linkSecretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s: the copy is still there (%v)", why, err)
		}
	}

	// A URL that only resolves in the database's own namespace is not copied.
	source(map[string][]byte{"uri": []byte("postgresql://app:two@main-rw:5432/app")})
	if op := f.deployRef(t, "staging", ""); op.GetError() == nil || !strings.Contains(op.GetError().GetMessage(), "fqdn-uri") {
		t.Errorf("a Secret without fqdn-uri: %v", op.GetError())
	}
	source(map[string][]byte{"fqdn-uri": []byte("postgresql://app:two@main-rw.ledger-staging.svc.cluster.local:5432/app")})

	// A part that links, or shares, cannot leave the application: it would
	// keep what the application gave it.
	f.catalog.EnableTeardown(f.deploys, nil, f.ops)
	leave := func(svc, want string) {
		t.Helper()
		_, err := f.catalog.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service:    &forgev1.Service{Name: "services/" + svc, Application: ""},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"application"}},
		}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
			t.Errorf("%s leaving: want a refusal with %q, got %v", svc, want, err)
		}
	}
	leave("payments", "links to parts of the application")
	leave("ledger", "shares its database main")

	// When the owner stops sharing, the copies go.
	var told []string
	f.deploys.revokeSharedDatabase(context.Background(), "ledger", "staging", func(_, msg string) { told = append(told, msg) })
	gone("after the owner stopped sharing")
	if len(told) != 1 || !strings.Contains(told[0], "services/payments") {
		t.Errorf("events: %q", told)
	}
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	copied()

	// Without the link, the copy goes.
	gh.Files[envSHA+":forge.yaml"] = []byte(base)
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if f.values(t, "staging").Database != nil {
		t.Error("the values still name a database")
	}
	gone("after the link was removed")
}
