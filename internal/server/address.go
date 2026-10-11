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

// publicListener is the Gateway listener the routes of public addresses attach to.
const publicListener = "public"

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
	// dns is set for a public address.
	dns *deployDNS
}

func (a address) url() string { return hostURL(a.host, a.dns != nil) }

// hostURL is the address people open: public addresses are served over HTTPS
// by the platform's edge, internal ones over plain HTTP.
func hostURL(host string, public bool) string {
	if public {
		return "https://" + host
	}
	return "http://" + host
}

// address works out where svc is reached in env. want is run.domain from the
// service's forge.yaml. Without public domains, or with "none", it is the
// internal address <service>.<env>.<domain>. Otherwise it is one label under
// the chosen domain, because a certificate for *.example.com covers nothing
// deeper: <service>.<domain> in production, <service>-<env>.<domain> elsewhere.
func (d *Deploys) address(svc, env, want string) (address, error) {
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
	label := svc
	if env != finalEnv {
		label = svc + "-" + env
	}
	// "payments-staging" in production would get the address of "payments" in staging.
	for _, e := range deployEnvs {
		if e != finalEnv && strings.HasSuffix(svc, "-"+e) {
			return refuse("a service named %s cannot have a public address: %s.%s is where the service %q is reached in %s. Rename it, or set run.domain to %q",
				svc, svc, domain.Name, strings.TrimSuffix(svc, "-"+e), e, manifest.DomainNone)
		}
	}
	if len(label) > 63 {
		return refuse("the address %s.%s is too long: a name before the domain has at most 63 characters", label, domain.Name)
	}
	return address{
		host: label + "." + domain.Name, listener: publicListener,
		dns: &deployDNS{Name: label, Zone: domain.Zone, Tunnel: d.o.DNSTunnel},
	}, nil
}

// hostFree refuses a public address that is new to svc/env and already
// resolves: something else is there (another system's record, a name taken
// by hand). An address the environment already has is its own.
func (d *Deploys) hostFree(ctx context.Context, svc, env string, a address) error {
	if a.dns == nil || d.o.HostTaken == nil {
		return nil
	}
	if cur, _, err := d.current(ctx, svc, env); err == nil && cur.Host == a.host {
		return nil
	}
	taken, err := d.o.HostTaken(ctx, a.host)
	if err != nil {
		d.o.Log.Warn("could not check whether an address is free", "host", a.host, "error", err)
		return nil // DNS being away is no reason to stop a deploy
	}
	if taken {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s is already in use (the name resolves, and not to %s%s in %s). Rename the service, pick another domain with run.domain in forge.yaml, or set it to %q",
			a.host, servicePrefix, svc, env, manifest.DomainNone))
	}
	return nil
}

// ResolvesInDNS reports whether host has an address in DNS. It is the
// HostTaken of a real Forge API.
func ResolvesInDNS(ctx context.Context, host string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false, nil
	}
	return len(addrs) > 0, err
}
