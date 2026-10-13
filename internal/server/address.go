package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/manifest"
)

// PublicDomain is a domain services can be reached under from outside.
type PublicDomain struct {
	// Name is the domain, e.g. "example.com".
	Name string
	// Zone names the domain's DNS zone to the platform's DNS records
	// (CloudflareRecordsApplication's dnsZones.zones), e.g. "example-com".
	Zone string
}

// ParsePublicDomains reads "example.com=example-com,other.io=other-io": the
// domains services may use with the name of each one's zone, the first being
// the default. An empty list means no public addresses.
func ParsePublicDomains(list string) ([]PublicDomain, error) {
	var out []PublicDomain
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		name, zone, ok := strings.Cut(item, "=")
		if !ok || !manifest.ValidDomain(name) || !manifest.ValidID(zone) {
			return nil, fmt.Errorf("public domain %q must be <domain>=<zone name>, like example.com=example-com", item)
		}
		if slices.ContainsFunc(out, func(d PublicDomain) bool { return d.Name == name }) {
			return nil, fmt.Errorf("public domain %s is listed twice", name)
		}
		out = append(out, PublicDomain{Name: name, Zone: zone})
	}
	return out, nil
}

// publicListener is the Gateway listener the routes of a public domain's
// addresses attach to: one per domain, which takes only hosts under it.
func publicListener(d PublicDomain) string { return "public-" + d.Zone }

// deployDNS is the DNS record of a public address, as the chart makes it.
type deployDNS struct {
	// Name is the host's first label; the record is <name>.<the zone's domain>.
	Name string `yaml:"name"`
	// Zone is the PublicDomain's zone name.
	Zone string `yaml:"zone"`
	// Tunnel says which of the platform's tunnels the record points at.
	Tunnel string `yaml:"tunnel"`
}

// address is where a service is reached in an environment.
type address struct {
	host string
	// listener is the Gateway listener of the route; "" is the environment's own.
	listener string
	// dns is set for a public address whose record Forge makes.
	dns *deployDNS
	// public is set for every address reachable from the internet, with or
	// without a record of Forge's (the root of a domain has none).
	public bool
}

func (a address) url() string { return hostURL(a.host, a.public) }

// hostURL is the address people open: public addresses are served over HTTPS
// by the platform's edge, internal ones over plain HTTP.
func hostURL(host string, public bool) string {
	if public {
		return "https://" + host
	}
	return "http://" + host
}

// wanted is the address a service asks for: forge.yaml's run.domain and
// run.host, each "" for the default, or the setting made in Forge, which
// wins (set). claimed are addresses in use the service may take over.
type wanted struct {
	domain, host string
	claimed      []string
	set          bool
}

// HostRoot is the host of a service reached at the domain itself.
const HostRoot = "@"

// rootListener is the Gateway listener of a domain's root: the public
// listener takes *.<domain>, which does not match the domain itself.
func rootListener(d PublicDomain) string { return publicListener(d) + "-root" }

// withSetting returns what svc asks for: the address set in Forge when there
// is one, else forge.yaml's (fromYAML). The note says when the two disagree.
func (d *Deploys) withSetting(ctx context.Context, svc string, fromYAML wanted) (wanted, string, error) {
	if d.o.Catalog == nil {
		return fromYAML, "", nil
	}
	s, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return fromYAML, "", nil
	}
	if err != nil {
		return fromYAML, "", err
	}
	a := s.GetAddress()
	if a == nil {
		return fromYAML, "", nil
	}
	w := wanted{domain: a.GetDomain(), host: a.GetHost(), claimed: a.GetClaimed(), set: true}
	note := ""
	if fromYAML.domain != w.domain || fromYAML.host != w.host {
		note = fmt.Sprintf("the address set in Forge (host %q, domain %q) wins over forge.yaml's (run.host %q, run.domain %q)", w.host, w.domain, fromYAML.host, fromYAML.domain)
	}
	return w, note, nil
}

// address works out where svc is reached in env. Without public domains, or
// with the domain "none", it is the internal address <service>.<env>.<domain>.
// Otherwise it is one label under the chosen domain, because a certificate
// for *.example.com covers nothing deeper: <name>.<domain> in production and
// <name>-<env>.<domain> elsewhere, where the name is the service's own unless
// it asks for another with run.host.
func (d *Deploys) address(svc, env string, w wanted) (address, error) {
	want := w.domain
	refuse := func(format string, a ...any) (address, error) {
		return address{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, a...))
	}
	if want == manifest.DomainNone || (len(d.o.PublicDomains) == 0 && want == "") {
		return address{host: d.host(svc, env)}, nil
	}
	if len(d.o.PublicDomains) == 0 {
		return refuse("forge.yaml asks for run.domain %s, but this Forge API offers no public domains; remove it or use %q", want, manifest.DomainNone)
	}
	domain := d.o.PublicDomains[0]
	if want != "" {
		i := slices.IndexFunc(d.o.PublicDomains, func(p PublicDomain) bool { return p.Name == want })
		if i < 0 {
			var offered []string
			for _, p := range d.o.PublicDomains {
				offered = append(offered, p.Name)
			}
			return refuse("run.domain %s is not a domain of this platform: use %s, or %q for no public address", want, strings.Join(offered, " or "), manifest.DomainNone)
		}
		domain = d.o.PublicDomains[i]
	}
	name, named := svc, "a service named "+svc
	if w.host != "" && w.host != svc {
		name, named = w.host, "run.host "+w.host
	}
	if name == HostRoot {
		// The root: <domain> in production, <env>.<domain> elsewhere. Forge
		// makes no record for the domain itself; it must point at the tunnel.
		if env == finalEnv {
			return address{host: domain.Name, listener: rootListener(domain), public: true}, nil
		}
		return address{
			host: env + "." + domain.Name, listener: publicListener(domain), public: true,
			dns: &deployDNS{Name: env, Zone: domain.Zone, Tunnel: d.o.DNSTunnel},
		}, nil
	}
	// <env>.<domain> is where the root of a domain is reached in that environment.
	if slices.Contains(deployEnvs, name) {
		return refuse("%s cannot have a public address: %s.%s is where the service at the root of %s is reached in %s. Pick another name", named, name, domain.Name, domain.Name, name)
	}
	label := name
	if env != finalEnv {
		label = name + "-" + env
	}
	// "payments-staging" in production would get the address of "payments" in staging.
	for _, e := range deployEnvs {
		if e != finalEnv && strings.HasSuffix(name, "-"+e) {
			return refuse("%s cannot have a public address: %s.%s is where %q is reached in %s. Pick another name, or set run.domain to %q",
				named, name, domain.Name, strings.TrimSuffix(name, "-"+e), e, manifest.DomainNone)
		}
	}
	if len(label) > 63 {
		return refuse("the address %s.%s is too long: a name before the domain has at most 63 characters", label, domain.Name)
	}
	return address{
		host: label + "." + domain.Name, listener: publicListener(domain), public: true,
		dns: &deployDNS{Name: label, Zone: domain.Zone, Tunnel: d.o.DNSTunnel},
	}, nil
}

// addressFor is address plus the checks a change of address needs: the new
// one must be free, and the caller is told what changes. current is what
// the environment has now, nil before its first deploy. The note says what
// the reader of the operation must know: that the service is public, and
// which address stops working. It is "" when nothing of that changes.
func (d *Deploys) addressFor(ctx context.Context, svc, env string, fromYAML wanted, current *deployValues) (address, string, error) {
	want, disagree, err := d.withSetting(ctx, svc, fromYAML)
	if err != nil {
		return address{}, "", err
	}
	a, err := d.address(svc, env, want)
	if err != nil {
		return a, "", err
	}
	if err := d.nameFree(ctx, svc, want); err != nil {
		return a, "", err
	}
	if current != nil && current.Host == a.host {
		return a, disagree, nil
	}
	if !slices.Contains(want.claimed, a.host) {
		if err := d.hostFree(ctx, a); err != nil {
			return a, "", err
		}
	}
	moved := current != nil && current.Host != ""
	if !a.public && !moved {
		return a, disagree, nil // a first deploy to an internal address: nothing to point out
	}
	note := a.url()
	if a.public {
		note += " (public: reachable from the internet)"
	}
	if moved {
		note += "; " + hostURL(current.Host, current.isPublic()) + " stops working"
	}
	if disagree != "" {
		note += "; " + disagree
	}
	return a, note, nil
}

// nameFree refuses a run.host that is the name of another service in the
// catalog: that service's addresses are its own, deployed yet or not.
func (d *Deploys) nameFree(ctx context.Context, svc string, w wanted) error {
	if w.host == "" || w.host == svc || w.host == HostRoot || w.domain == manifest.DomainNone || len(d.o.PublicDomains) == 0 || d.o.Catalog == nil {
		return nil
	}
	taken, err := d.o.Catalog.has(ctx, servicePrefix+w.host)
	if err != nil {
		return err
	}
	if taken {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"run.host %s is the name of another service, %s%s: its addresses are its own. Pick another name", w.host, servicePrefix, w.host))
	}
	return nil
}

// hostFree refuses a public address that already resolves: something else
// is there (another system's record, a name taken by hand). It is asked only
// for an address that is new to its environment. When DNS cannot be asked
// the address is not handed out: a deploy can wait, a hijacked name cannot
// be taken back.
func (d *Deploys) hostFree(ctx context.Context, a address) error {
	if !a.public || d.o.HostTaken == nil {
		return nil
	}
	taken, err := d.o.HostTaken(ctx, a.host)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("could not check whether %s is free: %w; try again", a.host, err))
	}
	if taken {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s is already in use (the name resolves in DNS). Rename the service, pick another domain with run.domain in forge.yaml, or set it to %q",
			a.host, manifest.DomainNone))
	}
	return nil
}

// ResolvesInDNS reports whether host has an address in DNS. It is the
// HostTaken of a real Forge API.
func ResolvesInDNS(ctx context.Context, host string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The trailing dot makes the name absolute: without it a resolver tries
	// its search domains first, and a wildcard in one of them would make
	// every name look taken.
	addrs, err := net.DefaultResolver.LookupHost(ctx, strings.TrimSuffix(host, ".")+".")
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false, nil
	}
	return len(addrs) > 0, err
}
