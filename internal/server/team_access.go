package server

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
)

// teamRepoRole is the role a service's owner team gets on its repo: enough
// to merge, manage issues and settings, but not to delete the repo or change
// who has access.
const teamRepoRole = "maintain"

// TeamAccess changes which teams have access to a repo. *github.Client
// implements it.
type TeamAccess interface {
	SetTeamRepoPermission(ctx context.Context, org, slug, owner, repo, permission string) error
	RemoveTeamRepo(ctx context.Context, org, slug, owner, repo string) error
}

// ManageRepoAccess makes the catalog give a service's owner team the role
// maintain on the service's repo in org, when the service is registered or
// moves to another team or repo. Without it the catalog leaves GitHub alone.
func (c *Catalog) ManageRepoAccess(t TeamAccess, org string) {
	c.access, c.accessOrg = t, org
}

// grantTeam gives team its role on the repo at repoURL. A repo outside the
// organization, or no team or repo, is left alone.
func (c *Catalog) grantTeam(ctx context.Context, team, repoURL string) error {
	if c.access == nil || team == "" {
		return nil
	}
	owner, repo, ok := repoOf(repoURL, c.accessOrg)
	if !ok {
		return nil
	}
	if err := c.access.SetTeamRepoPermission(ctx, c.accessOrg, team, owner, repo, teamRepoRole); err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("github: give team %s %s access to %s/%s: %w", team, teamRepoRole, owner, repo, err))
	}
	return nil
}

// revokeTeam takes team's access to the repo at repoURL away, unless another
// service of that repo is still owned by team. It only logs a failure: the
// change it follows has been made.
func (c *Catalog) revokeTeam(ctx context.Context, team, repoURL string) {
	if c.access == nil || team == "" {
		return
	}
	owner, repo, ok := repoOf(repoURL, c.accessOrg)
	if !ok {
		return
	}
	names, err := c.store.ForRepo(ctx, owner, repo)
	if err != nil {
		slog.Warn("team access kept: cannot list the repo's services", "team", team, "repo", owner+"/"+repo, "err", err)
		return
	}
	for _, n := range names {
		if s, _, err := c.store.Get(ctx, n); err != nil || s.GetOwnerTeam() == team {
			return
		}
	}
	if err := c.access.RemoveTeamRepo(ctx, c.accessOrg, team, owner, repo); err != nil {
		slog.Warn("cannot take the old team's access to the repo away", "team", team, "repo", owner+"/"+repo, "err", err)
	}
}
