package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
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
	for _, bad := range []string{"showmino.com", "showmino.com=", "=showmino-com", "Showmino.com=showmino-com", "showmino.com=showmino.com", "localhost=local", "a.com=a-com,a.com=other", "a..com=a-com", "*.a.com=a-com", "x.com=x-com,y.com=x-com-root"} {
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
		as             string // run.host
		internal       bool   // run.staging.public: false
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
		{name: "a name that is another service's staging address", d: pub, svc: "payments-staging", env: "production", refused: `a service named payments-staging cannot have a public address: payments-staging.showmino.com is where "payments" is reached in staging`},
		{name: "that name is fine without a public address", d: pub, svc: "payments-staging", env: "production", want: "none", host: "payments-staging.production.10.150.1.222.nip.io", url: "http://payments-staging.production.10.150.1.222.nip.io"},
		{name: "a name too long with its suffix", d: pub, svc: strings.Repeat("a", 60), env: "staging", refused: "too long"},
		// run.host: the name before the domain is not the service's own.
		{name: "another name, production", d: pub, svc: "forge-web", env: "production", as: "forge", host: "forge.showmino.com", url: "https://forge.showmino.com", dns: "forge showmino-com production"},
		{name: "another name, staging", d: pub, svc: "forge-web", env: "staging", as: "forge", host: "forge-staging.showmino.com", url: "https://forge-staging.showmino.com", dns: "forge-staging showmino-com production"},
		{name: "another name on the other domain", d: pub, svc: "forge-web", env: "production", as: "forge", want: "deogratias.cloud", host: "forge.deogratias.cloud", url: "https://forge.deogratias.cloud", dns: "forge deogratias-cloud production"},
		{name: "the service's own name as run.host", d: pub, svc: "payments", env: "production", as: "payments", host: "payments.showmino.com", url: "https://payments.showmino.com", dns: "payments showmino-com production"},
		{name: "another name that is someone's staging address", d: pub, svc: "forge-web", env: "production", as: "shop-staging", refused: `run.host shop-staging cannot have a public address`},
		{name: "another name means nothing for an internal address", d: pub, svc: "forge-web", env: "staging", as: "forge", want: "none", host: "forge-web.staging.10.150.1.222.nip.io", url: "http://forge-web.staging.10.150.1.222.nip.io"},
		{name: "another name without public domains", d: lan, svc: "forge-web", env: "staging", as: "forge", host: "forge-web.staging.10.150.1.222.nip.io", url: "http://forge-web.staging.10.150.1.222.nip.io"},
		// The root of a domain: the domain itself in production, <env>.<domain> elsewhere.
		{name: "the root, staging", d: pub, svc: "deogratias", env: "staging", as: "@", want: "deogratias.cloud", host: "staging.deogratias.cloud", url: "https://staging.deogratias.cloud", dns: "staging deogratias-cloud production"},
		{name: "an environment's name as the host", d: pub, svc: "web", env: "production", as: "staging", refused: "staging.showmino.com is where the service at the root of showmino.com is reached in staging"},
		{name: "the root means nothing for an internal address", d: pub, svc: "deogratias", env: "staging", as: "@", want: "none", host: "deogratias.staging.10.150.1.222.nip.io", url: "http://deogratias.staging.10.150.1.222.nip.io"},
		// run.staging.public: false keeps staging internal and leaves production as it is.
		{name: "staging kept internal", d: pub, svc: "payments", env: "staging", internal: true, host: "payments.staging.10.150.1.222.nip.io", url: "http://payments.staging.10.150.1.222.nip.io"},
		{name: "production public all the same", d: pub, svc: "payments", env: "production", internal: true, host: "payments.showmino.com", url: "https://payments.showmino.com", dns: "payments showmino-com production"},
		{name: "staging kept internal, another name and domain", d: pub, svc: "forge-web", env: "staging", as: "forge", want: "deogratias.cloud", internal: true, host: "forge-web.staging.10.150.1.222.nip.io", url: "http://forge-web.staging.10.150.1.222.nip.io"},
		{name: "staging kept internal, the root", d: pub, svc: "deogratias", env: "staging", as: "@", want: "deogratias.cloud", internal: true, host: "deogratias.staging.10.150.1.222.nip.io", url: "http://deogratias.staging.10.150.1.222.nip.io"},
		{name: "staging kept internal, a domain the platform does not have", d: pub, svc: "payments", env: "staging", want: "example.org", internal: true, refused: "use showmino.com or deogratias.cloud"},
	} {
		a, err := tc.d.address(tc.svc, tc.env, wanted{domain: tc.want, host: tc.as, stagingInternal: tc.internal})
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
		listener := ""
		if tc.dns != "" {
			listener = "public-" + strings.Fields(tc.dns)[1] // one listener per domain
		}
		if err != nil || a.host != tc.host || a.url() != tc.url || dns != tc.dns || a.listener != listener {
			t.Errorf("%s: %+v dns %q, %v", tc.name, a, dns, err)
		}
	}
	// The root in production: no record of Forge's, a listener of its own, and public all the same.
	a, err := pub.address("deogratias", "production", wanted{domain: "deogratias.cloud", host: "@"})
	if err != nil || a.host != "deogratias.cloud" || a.url() != "https://deogratias.cloud" || a.dns != nil || a.listener != "public-deogratias-cloud-root" || !a.public {
		t.Errorf("the root in production: %+v, %v", a, err)
	}
}

// taken is the set of public addresses that resolve already.
type taken struct {
	mu    sync.Mutex
	hosts map[string]bool
	asked []string
	// err, when set, is DNS not answering.
	err error
}

func (k *taken) check(_ context.Context, host string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, host)
	return k.hosts[host], k.err
}

func (k *taken) set(f func(*taken)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f(k)
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
	dns.set(func(k *taken) { k.hosts["payments-staging.showmino.com"] = true })
	for _, dry := range []bool{true, false} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: dry}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "payments-staging.showmino.com is already in use") {
			t.Fatalf("a taken address (dry run %v): %v", dry, err)
		}
	}
	// When DNS cannot be asked, a new address is not handed out.
	dns.set(func(k *taken) { k.hosts, k.err = map[string]bool{}, errors.New("no such host: timeout") })
	if _, err := f.start(t, "staging"); connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "could not check whether payments-staging.showmino.com is free") {
		t.Fatalf("DNS away: %v", err)
	}
	if _, err := f.repo.Read(ctx, valuesPath("payments", "staging"), ""); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("a refused deploy wrote values: %v", err)
	}
	dns.set(func(k *taken) { k.err = nil })

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
	// The operation says that the service is now on the internet.
	if got := stepMessage(op, "address"); got != "https://payments-staging.showmino.com (public: reachable from the internet)" {
		t.Errorf("address step %q", got)
	}
	v := values("staging")
	if v.Host != "payments-staging.showmino.com" || v.Listener != "public-showmino-com" || v.DNS == nil || *v.DNS != (deployDNS{Name: "payments-staging", Zone: "showmino-com", Tunnel: "production"}) {
		t.Errorf("staging values: %+v dns %+v", v, v.DNS)
	}
	// Its own address resolves by now, and DNS may even be away: a redeploy
	// to the same address asks nothing and says nothing about it.
	dns.set(func(k *taken) { k.hosts["payments-staging.showmino.com"], k.err = true, errors.New("timeout") })
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil || stepMessage(op, "address") != "" {
		t.Fatalf("redeploy: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	dns.set(func(k *taken) { k.err = nil })

	// Promotion gives production the service's own name.
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	v = values("production")
	if v.Host != "payments.showmino.com" || v.DNS == nil || v.DNS.Name != "payments" || v.Listener != "public-showmino-com" {
		t.Errorf("production values: %+v dns %+v", v, v.DNS)
	}
	got, err := f.client.GetDeployment(ctx, as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	if err != nil || got.Msg.GetDeployment().GetUrl() != "https://payments.showmino.com" {
		t.Errorf("production: %v %v", got, err)
	}

	// A commit whose forge.yaml picks the other domain moves staging there
	// (the operation names both addresses), and a promotion carries the
	// choice to production.
	op = f.deployRef(t, "staging", "release")
	if op.GetError() != nil || stepMessage(op, "address") != "https://payments-staging.deogratias.cloud (public: reachable from the internet); https://payments-staging.showmino.com stops working" {
		t.Fatalf("moving domain: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if v := values("staging"); v.Host != "payments-staging.deogratias.cloud" || v.DNS.Zone != "deogratias-cloud" || v.Listener != "public-deogratias-cloud" || v.Forge.Domain != "deogratias.cloud" {
		t.Errorf("staging on the other domain: %+v dns %+v", v, v.DNS)
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("production"); v.Host != "payments.deogratias.cloud" || v.DNS.Zone != "deogratias-cloud" {
		t.Errorf("production on the other domain: %+v dns %+v", v, v.DNS)
	}
	// A rollback restores the commit and with it the domain that commit
	// asked for, worked out again: here the default domain.
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/production"}))
	if err != nil {
		t.Fatal(err)
	}
	op = wait(t, f.ops, rb.Msg.GetOperation().GetName())
	if op.GetError() != nil || !strings.HasPrefix(stepMessage(op, "address"), "https://payments.showmino.com (public") {
		t.Fatalf("rollback: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if v := values("production"); v.Host != "payments.showmino.com" || v.DNS.Zone != "showmino-com" || v.Forge.Kind != kindRollback {
		t.Errorf("production after the rollback: %+v dns %+v", v, v.DNS)
	}
	// A rollback to an address that someone else has by now is refused.
	dns.set(func(k *taken) { k.hosts["payments.deogratias.cloud"] = true })
	if _, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/production"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a rollback to a taken address: %v", err)
	}
}

// Public domains are switched on for a platform whose services have
// internal addresses: nothing moves until a service is deployed again, and a
// rollback to a deploy from before does not take the service off the
// internet again.
func TestPublicAddressesTurnedOn(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	host := func(env string) string {
		t.Helper()
		data, err := f.repo.Read(ctx, valuesPath("payments", env), "")
		if err != nil {
			t.Fatal(err)
		}
		v, err := parseValues(data)
		if err != nil {
			t.Fatal(err)
		}
		return v.Host
	}
	f.deployOK(t, "staging")
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if host("staging") != "payments.staging.10.150.1.222.nip.io" || host("production") != "payments.production.10.150.1.222.nip.io" {
		t.Fatalf("before: %s %s", host("staging"), host("production"))
	}

	// The setting is switched on (a restart of forge-api with it).
	f.deploys.o.PublicDomains = []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}}
	list, err := f.client.ListDeployments(ctx, as("reader", &forgev1.ListDeploymentsRequest{}))
	if err != nil || len(list.Msg.GetDeployments()) != 2 || list.Msg.GetDeployments()[0].GetUrl() != "http://payments.staging.10.150.1.222.nip.io" {
		t.Fatalf("what runs keeps its address until it is deployed again: %v %v", list, err)
	}
	// The next promotion moves production, and says so.
	op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED)
	if op.GetError() != nil || stepMessage(op, "address") != "https://payments.showmino.com (public: reachable from the internet); http://payments.production.10.150.1.222.nip.io stops working" {
		t.Fatalf("promotion after the switch: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	// Rolling production back to the deploy from before keeps it public.
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/production"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil || stepMessage(op, "address") != "" {
		t.Fatalf("rollback: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if host("production") != "payments.showmino.com" {
		t.Errorf("production after the rollback: %s", host("production"))
	}
}

func TestResolvesInDNS(t *testing.T) {
	t.Parallel()
	// A name under a reserved top-level domain never resolves, and that is
	// "free", not an error.
	taken, err := ResolvesInDNS(context.Background(), "nothing-here.forge-test.invalid")
	if err != nil {
		t.Skipf("no DNS to ask here: %v", err)
	}
	if taken {
		t.Error("a name that cannot exist resolves")
	}
}

// A service may be reached under another name than its own (run.host), but
// not under the name of another service.
func TestPublicAddressOtherName(t *testing.T) {
	t.Parallel()
	dns := &taken{hosts: map[string]bool{}}
	manifest := func(run string) []byte {
		return []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n" + run)
	}
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.PublicDomains = []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}}
		o.HostTaken = dns.check
		files := o.GitHub.(*envtest.GitHub).Files
		files[envSHA+":forge.yaml"] = manifest("  host: pay\n")
		files[ciOldSHA+":forge.yaml"] = manifest("  host: ledger\n")
	})
	ctx := context.Background()
	if _, err := f.catalog.RegisterService(writerCtx(), connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/ledger"},
	})); err != nil {
		t.Fatal(err)
	}

	// The name of another service is refused, deployed or not, also in a dry run.
	for _, dry := range []bool{true, false} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "release", ValidateOnly: dry}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "run.host ledger is the name of another service") {
			t.Fatalf("another service's name (dry run %v): %v", dry, err)
		}
	}

	// A free name: staging and, by promotion, production carry it.
	op := f.deployRef(t, "staging", "")
	if op.GetError() != nil || stepMessage(op, "address") != "https://pay-staging.showmino.com (public: reachable from the internet)" {
		t.Fatalf("deploy: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil || !strings.HasPrefix(stepMessage(op, "address"), "https://pay.showmino.com ") {
		t.Fatalf("promote: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	data, err := f.repo.Read(ctx, valuesPath("payments", "production"), "")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := parseValues(data)
	if v.Host != "pay.showmino.com" || v.DNS == nil || v.DNS.Name != "pay" || v.Forge.Host != "pay" || v.Name != "payments" {
		t.Errorf("production values: %+v dns %+v", v, v.DNS)
	}
	// The DNS check asked for the names it handed out, not the service's own.
	dns.mu.Lock()
	defer dns.mu.Unlock()
	if strings.Join(dns.asked, " ") != "pay-staging.showmino.com pay.showmino.com" {
		t.Errorf("asked DNS for %v", dns.asked)
	}
}

// run.staging.public: false keeps staging on the internal address while
// production is public: through a promotion, an address set in Forge, and
// until a commit drops it.
func TestStagingInternal(t *testing.T) {
	t.Parallel()
	dns := &taken{hosts: map[string]bool{}}
	manifest := func(run string) []byte {
		return []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n" + run)
	}
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.PublicDomains = []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}}
		o.HostTaken = dns.check
		files := o.GitHub.(*envtest.GitHub).Files
		files[envSHA+":forge.yaml"] = manifest("  staging:\n    public: false\n")
		files[ciOldSHA+":forge.yaml"] = manifest("")
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

	// Staging: the internal address, no record, nothing to point out.
	op := f.deployRef(t, "staging", "")
	if op.GetError() != nil || stepMessage(op, "address") != "" {
		t.Fatalf("deploy: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if v := values("staging"); v.Host != "payments.staging.10.150.1.222.nip.io" || v.DNS != nil || v.Listener != "" || v.isPublic() || !v.Forge.StagingInternal {
		t.Errorf("staging: %+v dns %+v", v, v.DNS)
	}
	// Production, by promotion: public under the default domain.
	op = f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED)
	if op.GetError() != nil || !strings.HasPrefix(stepMessage(op, "address"), "https://payments.showmino.com (public") {
		t.Fatalf("promote: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if v := values("production"); v.Host != "payments.showmino.com" || v.DNS == nil || v.DNS.Name != "payments" || v.Listener != "public-showmino-com" {
		t.Errorf("production: %+v dns %+v", v, v.DNS)
	}

	// An address set in Forge renames the service; staging stays internal.
	resp, err := f.client.SetAddress(ctx, as("writer", &forgev1.SetAddressRequest{Service: "services/payments", Host: "pay", Domain: "showmino.com"}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range resp.Msg.GetAddresses() {
		got = append(got, p.GetEnv()+" "+p.GetUrl())
	}
	if want := "staging http://payments.staging.10.150.1.222.nip.io\nproduction https://pay.showmino.com"; strings.Join(got, "\n") != want {
		t.Errorf("planned:\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil || strings.Contains(stepMessage(op, "address"), "public") {
		t.Fatalf("redeploy: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if v := values("staging"); v.Host != "payments.staging.10.150.1.222.nip.io" || v.DNS != nil {
		t.Errorf("staging with the setting: %+v dns %+v", v, v.DNS)
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("production"); v.Host != "pay.showmino.com" || v.DNS == nil || v.DNS.Name != "pay" {
		t.Errorf("production with the setting: %+v dns %+v", v, v.DNS)
	}
	// A rollback of staging keeps it internal.
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("staging"); v.Host != "payments.staging.10.150.1.222.nip.io" || v.DNS != nil {
		t.Errorf("staging after a rollback: %+v dns %+v", v, v.DNS)
	}
	// DNS was never asked about a staging name: none was handed out.
	dns.mu.Lock()
	asked := strings.Join(dns.asked, " ")
	dns.mu.Unlock()
	if strings.Contains(asked, "staging") {
		t.Errorf("asked DNS for %s", asked)
	}

	// A commit without run.staging.public makes staging public, and says so.
	op = f.deployRef(t, "staging", "release")
	if op.GetError() != nil || stepMessage(op, "address") != "https://pay-staging.showmino.com (public: reachable from the internet); http://payments.staging.10.150.1.222.nip.io stops working; the address set in Forge (host \"pay\", domain \"showmino.com\") wins over forge.yaml's (run.host \"\", run.domain \"\")" {
		t.Fatalf("public again: %v, address step %q", op.GetError(), stepMessage(op, "address"))
	}
	if v := values("staging"); v.Host != "pay-staging.showmino.com" || v.DNS == nil || v.Forge.StagingInternal {
		t.Errorf("staging public again: %+v dns %+v", v, v.DNS)
	}

	// Internal again, then a rollback to the commit without the flag: staging
	// was made internal on purpose, and a rollback does not undo that.
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	rb, err = f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("staging"); v.Host != "payments.staging.10.150.1.222.nip.io" || v.DNS != nil || !v.Forge.StagingInternal || v.Forge.Commit != ciOldSHA {
		t.Errorf("staging rolled back past the flag: %+v dns %+v", v, v.DNS)
	}
}

func TestListDomains(t *testing.T) {
	t.Parallel()
	ctx := writerCtx()
	d := NewDeploys(DeployOptions{PublicDomains: []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}, {Name: "forgegate.io", Zone: "forgegate-io"}}})
	resp, err := d.ListDomains(ctx, connect.NewRequest(&forgev1.ListDomainsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range resp.Msg.GetDomains() {
		got = append(got, fmt.Sprintf("%s %v", p.GetName(), p.GetIsDefault()))
	}
	if want := []string{"showmino.com true", "forgegate.io false"}; !slices.Equal(got, want) {
		t.Errorf("domains = %v, want %v", got, want)
	}
	resp, err = NewDeploys(DeployOptions{}).ListDomains(ctx, connect.NewRequest(&forgev1.ListDomainsRequest{}))
	if err != nil || len(resp.Msg.GetDomains()) != 0 {
		t.Errorf("without public domains: %v, %v", resp, err)
	}
	if _, err := d.ListDomains(context.Background(), connect.NewRequest(&forgev1.ListDomainsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("signed out: %v", err)
	}
	var none *Deploys
	if _, err := none.ListDomains(ctx, connect.NewRequest(&forgev1.ListDomainsRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("without deploys: %v", err)
	}
}
