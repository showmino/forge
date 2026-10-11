package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

func TestParsePublicDomains(t *testing.T) {
	t.Parallel()
	got, err := ParsePublicDomains(" showmino.com=showmino-com, deogratias.cloud=deogratias-cloud ,")
	if err != nil || len(got) != 2 || got[0] != (PublicDomain{Name: "showmino.com", Zone: "showmino-com"}) || got[1].Name != "deogratias.cloud" {
		t.Fatalf("got %+v %v", got, err)
	}
	if got, err := ParsePublicDomains(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %+v %v", got, err)
	}
	for _, bad := range []string{"showmino.com", "showmino.com=", "=showmino-com", "Showmino.com=showmino-com", "showmino.com=showmino.com", "localhost=local", "a.com=a-com,a.com=other", "a..com=a-com", "*.a.com=a-com"} {
		if got, err := ParsePublicDomains(bad); err == nil {
			t.Errorf("%q: got %+v, want an error", bad, got)
		}
	}
}

func TestAddress(t *testing.T) {
	t.Parallel()
	public := []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}, {Name: "deogratias.cloud", Zone: "deogratias-cloud"}}
	lan := NewDeploys(DeployOptions{Domain: "10.150.1.222.nip.io"})
	pub := NewDeploys(DeployOptions{Domain: "10.150.1.222.nip.io", PublicDomains: public})
	for _, tc := range []struct {
		name           string
		d              *Deploys
		svc, env, want string
		host, url      string
		dns            string // "<name> <zone> <tunnel>", "" for none
		refused        string // in the error
	}{
		{name: "no public domains", d: lan, svc: "payments", env: "staging", host: "payments.staging.10.150.1.222.nip.io", url: "http://payments.staging.10.150.1.222.nip.io"},
		{name: "none without public domains", d: lan, svc: "payments", env: "production", want: "none", host: "payments.production.10.150.1.222.nip.io", url: "http://payments.production.10.150.1.222.nip.io"},
		{name: "a domain without public domains", d: lan, svc: "payments", env: "staging", want: "showmino.com", refused: "offers no public domains"},
		{name: "the default domain, production", d: pub, svc: "payments", env: "production", host: "payments.showmino.com", url: "https://payments.showmino.com", dns: "payments showmino-com production"},
		{name: "the default domain, staging", d: pub, svc: "payments", env: "staging", host: "payments-staging.showmino.com", url: "https://payments-staging.showmino.com", dns: "payments-staging showmino-com production"},
		{name: "the other domain", d: pub, svc: "payments", env: "production", want: "deogratias.cloud", host: "payments.deogratias.cloud", url: "https://payments.deogratias.cloud", dns: "payments deogratias-cloud production"},
		{name: "none keeps the internal address", d: pub, svc: "forge-web", env: "staging", want: "none", host: "forge-web.staging.10.150.1.222.nip.io", url: "http://forge-web.staging.10.150.1.222.nip.io"},
		{name: "a domain the platform does not have", d: pub, svc: "payments", env: "staging", want: "example.org", refused: "use showmino.com or deogratias.cloud"},
		{name: "a name that is another service's staging address", d: pub, svc: "payments-staging", env: "production", refused: `where the service "payments" is reached in staging`},
		{name: "that name is fine without a public address", d: pub, svc: "payments-staging", env: "production", want: "none", host: "payments-staging.production.10.150.1.222.nip.io", url: "http://payments-staging.production.10.150.1.222.nip.io"},
		{name: "a name too long with its suffix", d: pub, svc: strings.Repeat("a", 60), env: "staging", refused: "too long"},
	} {
		a, err := tc.d.address(tc.svc, tc.env, tc.want)
		if tc.refused != "" {
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%s: %v, want a refusal with %q", tc.name, err, tc.refused)
			}
			continue
		}
		dns := ""
		if a.dns != nil {
			dns = a.dns.Name + " " + a.dns.Zone + " " + a.dns.Tunnel
		}
		if err != nil || a.host != tc.host || a.url() != tc.url || dns != tc.dns || (a.listener == publicListener) != (tc.dns != "") {
			t.Errorf("%s: %+v dns %q, %v", tc.name, a, dns, err)
		}
	}
}

// taken is the set of public addresses that resolve already.
type taken struct {
	mu    sync.Mutex
	hosts map[string]bool
	asked []string
}

func (k *taken) check(_ context.Context, host string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, host)
	return k.hosts[host], nil
}

func TestPublicAddresses(t *testing.T) {
	t.Parallel()
	dns := &taken{hosts: map[string]bool{}}
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.PublicDomains = []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}, {Name: "deogratias.cloud", Zone: "deogratias-cloud"}}
		o.HostTaken = dns.check
		// The commit behind "release" asks for the other domain.
		o.GitHub.(*envtest.GitHub).Files[ciOldSHA+":forge.yaml"] = []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n  domain: deogratias.cloud\n")
	})
	ctx := context.Background()
	values := func(env string) deployValues {
		t.Helper()
		data, err := f.repo.Read(ctx, valuesPath("payments", env), "")
		if err != nil {
			t.Fatal(err)
		}
		v, err := parseValues(data)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// A name something else has is refused before anything is written, also in a dry run.
	dns.hosts["payments-staging.showmino.com"] = true
	for _, dry := range []bool{true, false} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: dry}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "payments-staging.showmino.com is already in use") {
			t.Fatalf("a taken address (dry run %v): %v", dry, err)
		}
	}
	dns.hosts = map[string]bool{}

	// Staging gets one label under the default domain, over HTTPS, with its DNS record.
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if d := resp.GetDeployment(); d.GetUrl() != "https://payments-staging.showmino.com" {
		t.Errorf("preview: %v", d)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() != nil || stepMessage(op, "sync") != "https://payments-staging.showmino.com" {
		t.Fatalf("deploy: %v, sync step %q", op.GetError(), stepMessage(op, "sync"))
	}
	v := values("staging")
	if v.Host != "payments-staging.showmino.com" || v.Listener != "public" || v.DNS == nil || *v.DNS != (deployDNS{Name: "payments-staging", Zone: "showmino-com", Tunnel: "production"}) {
		t.Errorf("staging values: %+v dns %+v", v, v.DNS)
	}
	// Its own address resolves by now: a redeploy does not trip over it.
	dns.hosts["payments-staging.showmino.com"] = true
	f.deployOK(t, "staging")

	// Promotion gives production the service's own name.
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	v = values("production")
	if v.Host != "payments.showmino.com" || v.DNS == nil || v.DNS.Name != "payments" || v.Listener != "public" {
		t.Errorf("production values: %+v dns %+v", v, v.DNS)
	}
	got, err := f.client.GetDeployment(ctx, as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	if err != nil || got.Msg.GetDeployment().GetUrl() != "https://payments.showmino.com" {
		t.Errorf("production: %v %v", got, err)
	}

	// A commit whose forge.yaml picks the other domain moves staging there,
	// and a promotion carries the choice to production.
	if op := f.deployRef(t, "staging", "release"); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("staging"); v.Host != "payments-staging.deogratias.cloud" || v.DNS.Zone != "deogratias-cloud" || v.Forge.Domain != "deogratias.cloud" {
		t.Errorf("staging on the other domain: %+v dns %+v", v, v.DNS)
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("production"); v.Host != "payments.deogratias.cloud" || v.DNS.Zone != "deogratias-cloud" {
		t.Errorf("production on the other domain: %+v dns %+v", v, v.DNS)
	}
	// A rollback brings back the address of the deploy it restores.
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/production"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("production"); v.Host != "payments.showmino.com" || v.DNS.Zone != "showmino-com" {
		t.Errorf("production after the rollback: %+v dns %+v", v, v.DNS)
	}
}
