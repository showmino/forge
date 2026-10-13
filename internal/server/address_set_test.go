package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

func TestSetAddress(t *testing.T) {
	t.Parallel()
	dns := &taken{hosts: map[string]bool{"deogratias.cloud": true}}
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.PublicDomains = []PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}, {Name: "deogratias.cloud", Zone: "deogratias-cloud"}}
		o.HostTaken = dns.check
	})
	ctx := context.Background()
	set := func(who string, r *forgev1.SetAddressRequest) (*forgev1.SetAddressResponse, error) {
		r.Service = "services/payments"
		resp, err := f.client.SetAddress(ctx, as(who, r))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
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
	stored := func() *forgev1.ServiceAddress {
		t.Helper()
		s, err := f.catalog.get(ctx, "services/payments")
		if err != nil {
			t.Fatal(err)
		}
		return s.GetAddress()
	}

	// Staging first, at the address forge.yaml gives.
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}

	if _, err := set("reader", &forgev1.SetAddressRequest{Host: "@", Domain: "deogratias.cloud"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a reader: %v", err)
	}
	for _, bad := range []*forgev1.SetAddressRequest{
		{Host: "Shop"},
		{Host: "@", Domain: "none"},
		{Host: "staging", Domain: "deogratias.cloud"},
		{Domain: "example.org"},
		{Clear: true, Host: "@"},
	} {
		if _, err := set("writer", bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}

	// The dry run says where each environment goes and what is in use, and changes nothing.
	plan, err := set("writer", &forgev1.SetAddressRequest{Host: "@", Domain: "deogratias.cloud", ValidateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range plan.GetAddresses() {
		got = append(got, p.GetEnv()+" "+p.GetUrl()+" in_use="+map[bool]string{true: "yes", false: "no"}[p.GetInUse()]+" now="+p.GetCurrentUrl())
	}
	want := []string{
		"staging https://staging.deogratias.cloud in_use=no now=https://payments-staging.showmino.com",
		"production https://deogratias.cloud in_use=yes now=",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if stored() != nil {
		t.Fatal("a dry run stored the address")
	}

	// An address in use is taken over only when it is claimed.
	if _, err := set("writer", &forgev1.SetAddressRequest{Host: "@", Domain: "deogratias.cloud"}); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "deogratias.cloud is already in use") {
		t.Fatalf("unclaimed: %v", err)
	}
	if _, err := set("writer", &forgev1.SetAddressRequest{Host: "@", Domain: "deogratias.cloud", Claim: []string{"deogratias.cloud"}}); err != nil {
		t.Fatal(err)
	}
	if a := stored(); a.GetHost() != "@" || a.GetDomain() != "deogratias.cloud" || !slices.Equal(a.GetClaimed(), []string{"deogratias.cloud"}) {
		t.Errorf("stored: %v", a)
	}
	// UpdateService cannot change it, and a service read back keeps it.
	if _, err := f.catalog.UpdateService(writerCtx(), connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/payments", Address: &forgev1.ServiceAddress{Host: "shop"}}, UpdateMask: fieldMask("address"),
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("UpdateService set the address: %v", err)
	}

	// Nothing moves until a deploy; then the setting wins over forge.yaml, and says so.
	if v := values("staging"); v.Host != "payments-staging.showmino.com" {
		t.Errorf("staging moved before a deploy: %s", v.Host)
	}
	op := f.deployRef(t, "staging", "")
	if op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	note := stepMessage(op, "address")
	if !strings.HasPrefix(note, "https://staging.deogratias.cloud (public") || !strings.Contains(note, "https://payments-staging.showmino.com stops working") || !strings.Contains(note, "set in Forge") {
		t.Errorf("address step %q", note)
	}
	if v := values("staging"); v.Host != "staging.deogratias.cloud" || v.DNS == nil || v.DNS.Name != "staging" || v.Listener != "public-deogratias-cloud" {
		t.Errorf("staging: %+v dns %+v", v, v.DNS)
	}
	// Production at the root: claimed, so its DNS answer does not stop it; no record of Forge's.
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	v := values("production")
	if v.Host != "deogratias.cloud" || v.DNS != nil || !v.Public || v.Listener != "public-deogratias-cloud-root" {
		t.Errorf("production: %+v dns %+v", v, v.DNS)
	}
	d, err := f.client.GetDeployment(ctx, as("reader", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	if err != nil || d.Msg.GetDeployment().GetUrl() != "https://deogratias.cloud" {
		t.Errorf("production url: %v %v", d, err)
	}

	// Clearing goes back to forge.yaml at the next deploy.
	if _, err := set("writer", &forgev1.SetAddressRequest{Clear: true}); err != nil {
		t.Fatal(err)
	}
	if stored() != nil {
		t.Error("clear left the address")
	}
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if v := values("staging"); v.Host != "payments-staging.showmino.com" {
		t.Errorf("staging after clear: %s", v.Host)
	}
}
