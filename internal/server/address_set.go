package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/store"
)

// SetAddress changes where a service is reached: the name before the domain
// and the domain, overriding forge.yaml's run.host and run.domain. It changes
// no deployment; each environment moves at its next deploy. An address that
// is in use already (it resolves in DNS) is refused unless the request
// claims it, and the claim is kept on the service. A claim covers names
// outside Forge only: an address another service has, or has set, is
// refused all the same.
func (d *Deploys) SetAddress(ctx context.Context, req *connect.Request[forgev1.SetAddressRequest]) (*connect.Response[forgev1.SetAddressResponse], error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	m := req.Msg
	svc, ok := strings.CutPrefix(m.GetService(), servicePrefix)
	if !ok || !manifest.ValidID(svc) {
		return nil, invalidArg("service %q must be services/<name>", m.GetService())
	}
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	host, domain := strings.TrimSpace(m.GetHost()), strings.TrimSpace(m.GetDomain())
	if m.GetClear() && (host != "" || domain != "" || len(m.GetClaim()) > 0) {
		return nil, invalidArg("clear goes back to forge.yaml: it takes no host, domain or claim")
	}
	if host != "" && host != HostRoot && !manifest.ValidID(host) {
		return nil, invalidArg("host %q must be a lowercase DNS label (a-z, 0-9, '-'), or %q for the domain itself", host, HostRoot)
	}
	if domain != "" && domain != manifest.DomainNone && !manifest.ValidDomain(domain) {
		return nil, invalidArg("domain %q must be a domain such as example.com, or %q", domain, manifest.DomainNone)
	}
	if host == HostRoot && domain == manifest.DomainNone {
		return nil, invalidArg("the root of a domain needs a public domain, not %q", manifest.DomainNone)
	}
	if _, err := d.o.Catalog.get(ctx, servicePrefix+svc); err != nil {
		return nil, err
	}

	var planned []*forgev1.PlannedAddress
	var inUse []string
	for _, env := range deployEnvs {
		cur := d.currentValues(ctx, svc, env)
		want := wanted{domain: domain, host: host, set: true}
		if m.GetClear() {
			// Back to forge.yaml: what it asked for at the last deploy here.
			want = wanted{}
			if cur != nil {
				want = wanted{domain: cur.Forge.Domain, host: cur.Forge.Host}
			}
		}
		a, err := d.address(svc, env, want)
		if err != nil {
			return nil, err
		}
		if err := d.nameFree(ctx, svc, want); err != nil {
			return nil, err
		}
		p := &forgev1.PlannedAddress{Env: env, Url: a.url(), Public: a.public, Host: a.host}
		if cur != nil && cur.Host != "" {
			p.CurrentUrl = hostURL(cur.Host, cur.isPublic())
		}
		if a.public && !m.GetClear() {
			if err := d.heldByOther(ctx, svc, a.host); err != nil {
				return nil, err
			}
		}
		// Going back to forge.yaml claims nothing: its deploy checks DNS as usual.
		if a.public && !m.GetClear() && (cur == nil || cur.Host != a.host) && d.o.HostTaken != nil {
			taken, err := d.o.HostTaken(ctx, a.host)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("could not check whether %s is free: %w; try again", a.host, err))
			}
			p.InUse = taken
			if taken {
				inUse = append(inUse, a.host)
			}
		}
		planned = append(planned, p)
	}
	if m.GetValidateOnly() {
		s, err := d.o.Catalog.get(ctx, servicePrefix+svc)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&forgev1.SetAddressResponse{Service: s, Addresses: planned}), nil
	}
	var unclaimed []string
	for _, h := range inUse {
		if !slices.Contains(m.GetClaim(), h) {
			unclaimed = append(unclaimed, h)
		}
	}
	if len(unclaimed) > 0 {
		verb := "is"
		if len(unclaimed) > 1 {
			verb = "are"
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s %s already in use (it resolves in DNS). Taking it over sends its visitors to %s: name it in claim to do so",
			strings.Join(unclaimed, " and "), verb, servicePrefix+svc))
	}
	var next *forgev1.ServiceAddress
	if !m.GetClear() {
		next = &forgev1.ServiceAddress{Host: host, Domain: domain, Claimed: inUse}
	}
	s, err := d.o.Catalog.setAddress(ctx, servicePrefix+svc, next, m.GetEtag())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.SetAddressResponse{Service: s, Addresses: planned}), nil
}

// setAddress stores a service's address setting; nil clears it. A non-empty
// etag must be the service's current one.
func (c *Catalog) setAddress(ctx context.Context, name string, a *forgev1.ServiceAddress, wantEtag string) (*forgev1.Service, error) {
	var out *forgev1.Service
	err := c.store.Write(ctx, func(tx store.Tx) error {
		cur, rev, err := tx.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		if wantEtag != "" && wantEtag != cur.GetEtag() {
			return connect.NewError(connect.CodeAborted, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, wantEtag, cur.GetEtag()))
		}
		next := cur
		next.Address = a
		next.UpdateTime = timestamppb.New(c.now())
		next.Etag = etag(rev + 1)
		out = next
		return tx.Put(ctx, next, rev+1)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return out, nil
}
