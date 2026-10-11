package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/semver"
)

// ReleaseGitHub is what cutting versions needs from GitHub (*github.Client).
type ReleaseGitHub interface {
	ListTagCommits(ctx context.Context, owner, repo string) ([]github.Tag, error)
	CreateTag(ctx context.Context, owner, repo, tag, sha string) error
	ReleaseExists(ctx context.Context, owner, repo, tag string) (bool, error)
	CreateRelease(ctx context.Context, owner, repo string, r github.Release) error
}

// finalEnv is the environment whose deploys are final versions; deploys to
// any other are release candidates.
const finalEnv = "production"

func bumpOf(b forgev1.Bump) (semver.Bump, error) {
	switch b {
	case forgev1.Bump_BUMP_UNSPECIFIED, forgev1.Bump_BUMP_PATCH:
		return semver.Patch, nil
	case forgev1.Bump_BUMP_MINOR:
		return semver.Minor, nil
	case forgev1.Bump_BUMP_MAJOR:
		return semver.Major, nil
	}
	return 0, invalidArg("bump %d must be patch, minor or major", b)
}

// versionPlan is the version a commit runs as in an environment.
type versionPlan struct {
	version semver.Version
	// tagged: the commit has this tag already, so nothing new is cut.
	tagged bool
	// previous is the tag the release notes start from; empty for the first.
	previous string
}

// planVersion works out commit's version from the repo's tags. A commit
// keeps a version it has: its final version if there is one, and for
// staging its candidate. Otherwise it gets the next one.
func planVersion(tags []github.Tag, commit string, final bool, bump semver.Bump) versionPlan {
	var all, here []semver.Version
	for _, t := range tags {
		v, ok := semver.Parse(t.Name)
		if !ok {
			continue
		}
		all = append(all, v)
		if t.Commit == commit && (v.Final() || !final) {
			here = append(here, v)
		}
	}
	p := versionPlan{}
	if p.version, p.tagged = semver.Highest(here); !p.tagged {
		if final {
			p.version = semver.NextFinal(all, bump)
		} else {
			p.version = semver.NextCandidate(all)
		}
	}
	if prev, ok := semver.Previous(all, p.version); ok {
		p.previous = prev.String()
	}
	return p
}

// serviceRepo is the GitHub repo of svc's code.
func (d *Deploys) serviceRepo(ctx context.Context, svc string) (string, string, error) {
	entry, err := d.o.Catalog.get(ctx, servicePrefix+svc)
	if err != nil {
		return "", "", err
	}
	owner, repo, ok := repoOf(entry.GetRepoUrl(), d.o.Org)
	if !ok {
		return "", "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s%s has no GitHub repo in %s (repo_url %q)", servicePrefix, svc, d.o.Org, entry.GetRepoUrl()))
	}
	return owner, repo, nil
}

// plannedVersion is the version a deploy of commit to env would run as,
// without cutting it. Empty when this Forge API cuts no versions.
func (d *Deploys) plannedVersion(ctx context.Context, owner, repo, commit, env string, bump semver.Bump) (string, error) {
	if d.o.Releases == nil {
		return "", nil
	}
	tags, err := d.o.Releases.ListTagCommits(ctx, owner, repo)
	if err != nil {
		return "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the tags of github.com/%s/%s: %w", owner, repo, err))
	}
	return planVersion(tags, commit, env == finalEnv, bump).version.String(), nil
}

// cutVersion makes sure commit has its version for env, as a tag and a
// GitHub release, and returns it: a release candidate for staging, a final
// version for production. Every step can be repeated, so a deploy that
// failed here is simply run again. Empty when this Forge API cuts no
// versions.
func (d *Deploys) cutVersion(ctx context.Context, owner, repo, commit, env string, bump semver.Bump, emit func(string, string)) (string, error) {
	if d.o.Releases == nil {
		return "", nil
	}
	// One at a time: two deploys must not work out the same next version.
	d.cutMu.Lock()
	defer d.cutMu.Unlock()
	failed := func(what string, err error) error {
		return &OpError{Code: "release_failed", Message: fmt.Sprintf("%s in github.com/%s/%s: %v", what, owner, repo, err)}
	}
	final := env == finalEnv
	for range 3 {
		tags, err := d.o.Releases.ListTagCommits(ctx, owner, repo)
		if err != nil {
			return "", failed("read the tags", err)
		}
		p := planVersion(tags, commit, final, bump)
		tag := p.version.String()
		if !p.tagged {
			err := d.o.Releases.CreateTag(ctx, owner, repo, tag, commit)
			if errors.Is(err, github.ErrTagExists) {
				continue // taken since the tags were read: work it out again
			}
			if err != nil {
				return "", failed("tag "+shortSHA(commit)+" as "+tag, err)
			}
		} else if ok, err := d.o.Releases.ReleaseExists(ctx, owner, repo, tag); err != nil {
			return "", failed("read the release "+tag, err)
		} else if ok {
			emit("release", fmt.Sprintf("%s is %s", shortSHA(commit), tag))
			return tag, nil
		}
		if err := d.o.Releases.CreateRelease(ctx, owner, repo, github.Release{Tag: tag, Commit: commit, Previous: p.previous, Prerelease: !final}); err != nil {
			return "", failed("create the release "+tag, err)
		}
		kind := "released"
		if !final {
			kind = "release candidate"
		}
		emit("release", fmt.Sprintf("%s %s for %s", kind, tag, shortSHA(commit)))
		return tag, nil
	}
	return "", failed("find a free version for "+shortSHA(commit), errors.New("the next tag was taken three times"))
}

// shownVersion is the version to show for env's deployment: staging shows
// the final version once production runs the same commit as it.
func shownVersion(env string, v deployValues, production *deployValues) string {
	if env != finalEnv && production != nil && production.Forge.Commit == v.Forge.Commit && production.Forge.Version != "" {
		return production.Forge.Version
	}
	return v.Forge.Version
}
