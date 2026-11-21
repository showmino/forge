package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// orgNameFresh is how long the organization's display name is served from
// memory; orgNameRetry how long after GitHub failed to give it.
const (
	orgNameFresh = time.Hour
	orgNameRetry = time.Minute
	orgNameWait  = 5 * time.Second
)

// OrgNamer reads a GitHub organization's display name; *github.Client is
// one. A TeamSource that is also one gives AuthService.GetStatus the name.
type OrgNamer interface {
	OrgName(ctx context.Context, org string) (string, error)
}

// orgName is the display name of the organization Forge serves, as the
// dashboard shows it, asked of GitHub at most once an hour.
type orgName struct {
	src OrgNamer
	org string
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	name  string
	until time.Time
}

// get returns the display name: the one GitHub gave last, or the slug when
// it gave none. Empty without an organization.
func (o *orgName) get(ctx context.Context) string {
	if o.org == "" {
		return ""
	}
	if o.src == nil {
		return o.org
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if now := o.now(); now.Before(o.until) {
		return o.orSlug()
	}
	// GetStatus waits for this: keep it short, and let a failure wait a minute.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orgNameWait)
	defer cancel()
	name, err := o.src.OrgName(ctx, o.org)
	if err != nil {
		o.log.WarnContext(ctx, "reading the organization's name failed", "org", o.org, "error", err)
		o.until = o.now().Add(orgNameRetry)
		return o.orSlug()
	}
	o.name, o.until = strings.TrimSpace(name), o.now().Add(orgNameFresh)
	return o.orSlug()
}

func (o *orgName) orSlug() string {
	if o.name == "" {
		return o.org
	}
	return o.name
}
