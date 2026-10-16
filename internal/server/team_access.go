package server

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/store"
)

// teamRepoRole is the role a service's owner team gets on its repo: enough
// to merge, manage issues and settings, but not to delete the repo or change
// who has access.
const teamRepoRole = "maintain"

// repoRoles are GitHub's repo roles, lowest first.
var repoRoles = []string{"", "pull", "triage", "push", "maintain", "admin"}

// TeamAccess reads and changes which teams have access to a repo.
// *github.Client implements it.
type TeamAccess interface {
	// TeamRepoPermission returns the team's highest role, "" for none.
	TeamRepoPermission(ctx context.Context, org, slug, owner, repo string) (string, error)
	SetTeamRepoPermission(ctx context.Context, org, slug, owner, repo, permission string) error
	RemoveTeamRepo(ctx context.Context, org, slug, owner, repo string) error
}

// ManageRepoAccess makes the catalog give a service's owner team the role
// maintain on the service's repo in org, when the service is registered or
// moves to another team or repo. Without it the catalog leaves GitHub alone.
func (c *Catalog) ManageRepoAccess(t TeamAccess, org string) {
	c.access, c.accessOrg = t, org
}

// undo reverses a change on GitHub when the write it belongs to fails.
type undo func()

func (u undo) run() {
	if u != nil {
		u()
	}
}

// grantTeam makes sure team has at least maintain on the repo at repoURL,
// and never lowers a higher role it has. A repo outside the organization,
// or no team or repo, is left alone. It is called inside a store write, so
// that grants and revokes run one at a time; the returned undo puts the
// team's old role back when that write fails.
func (c *Catalog) grantTeam(ctx context.Context, team, repoURL string) (undo, error) {
	if c.access == nil || team == "" {
		return nil, nil
	}
	owner, repo, ok := repoOf(repoURL, c.accessOrg)
	if !ok {
		return nil, nil
	}
	// The change on GitHub must not stop half way because the client left.
	ctx = context.WithoutCancel(ctx)
	failed := func(err error) error {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("github: give team %s %s access to %s/%s: %w", team, teamRepoRole, owner, repo, err))
	}
	had, err := c.access.TeamRepoPermission(ctx, c.accessOrg, team, owner, repo)
	if err != nil {
		return nil, failed(err)
	}
	if slices.Index(repoRoles, had) >= slices.Index(repoRoles, teamRepoRole) {
		return nil, nil
	}
	if err := c.access.SetTeamRepoPermission(ctx, c.accessOrg, team, owner, repo, teamRepoRole); err != nil {
		return nil, failed(err)
	}
	return func() {
		var err error
		if had == "" {
			err = c.access.RemoveTeamRepo(ctx, c.accessOrg, team, owner, repo)
		} else {
			err = c.access.SetTeamRepoPermission(ctx, c.accessOrg, team, owner, repo, had)
		}
		if err != nil {
			slog.Warn("cannot undo a team's access to a repo", "team", team, "repo", owner+"/"+repo, "role", had, "err", err)
		}
	}, nil
}

// revokeTeam takes away the maintain role team has on the repo at repoURL,
// unless a service of that repo still belongs to team. It is called inside
// the store write that moved the service away, after its Put, so tx sees the
// move. A team whose role is not maintain got it elsewhere and keeps it. A
// failure is only logged: the team keeps more access than it needs, which a
// person can take away. The returned undo gives the role back.
func (c *Catalog) revokeTeam(ctx context.Context, tx store.Tx, team, repoURL string) undo {
	if c.access == nil || team == "" {
		return nil
	}
	owner, repo, ok := repoOf(repoURL, c.accessOrg)
	if !ok {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	log := slog.With("team", team, "repo", owner+"/"+repo)
	names, err := tx.ForRepo(ctx, owner, repo)
	if err != nil {
		log.Warn("team access kept: cannot list the repo's services", "err", err)
		return nil
	}
	for _, n := range names {
		if s, _, err := tx.Get(ctx, n); err != nil || s.GetOwnerTeam() == team {
			return nil
		}
	}
	had, err := c.access.TeamRepoPermission(ctx, c.accessOrg, team, owner, repo)
	if err != nil {
		log.Warn("team access kept: cannot read it", "err", err)
		return nil
	}
	if had != teamRepoRole {
		return nil
	}
	if err := c.access.RemoveTeamRepo(ctx, c.accessOrg, team, owner, repo); err != nil {
		log.Warn("cannot take the old team's access to the repo away", "err", err)
		return nil
	}
	return func() {
		if err := c.access.SetTeamRepoPermission(ctx, c.accessOrg, team, owner, repo, teamRepoRole); err != nil {
			log.Warn("cannot give the team its access back", "err", err)
		}
	}
}
