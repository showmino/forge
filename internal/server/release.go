package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

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

// bumpOf reads a request's bump for a deploy to env. Only a deploy to
// production cuts a final version, so only there is there something to pick.
func bumpOf(b forgev1.Bump, env string) (semver.Bump, error) {
	if b != forgev1.Bump_BUMP_UNSPECIFIED && env != finalEnv {
		return 0, invalidArg("bump is for a deploy to %s: a deploy to %s is a release candidate", finalEnv, env)
	}
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
	// latest: no final version is higher, so its release is the repo's latest.
	latest bool
}

// planVersion works out commit's version from the repo's tags. A commit
// keeps a version it has: its final version if there is one, and for
// staging its candidate, unless that candidate's version was released from
// another commit since. Otherwise it gets the next one.
func planVersion(tags []github.Tag, commit string, final bool, bump semver.Bump) versionPlan {
	var all []semver.Version
	released := map[semver.Version]bool{}
	for _, t := range tags {
		if v, ok := semver.Parse(t.Name); ok {
			all = append(all, v)
			released[v] = v.Final()
		}
	}
	var here []semver.Version
	for _, t := range tags {
		v, ok := semver.Parse(t.Name)
		if ok && t.Commit == commit && (v.Final() || (!final && !released[v.Base()])) {
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
	last, ok := semver.LastFinal(all)
	p.latest = p.version.Final() && (!ok || semver.Compare(p.version, last) >= 0)
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

// cutLock is the lock held while a version is worked out and tagged in one
// repo: two deploys must not work out the same next version.
func (d *Deploys) cutLock(repo string) *sync.Mutex {
	m, _ := d.cutLocks.LoadOrStore(repo, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// cutVersion makes sure commit has its version for env and returns it: a
// release candidate for staging, a final version for production. The tag is
// the version, so a deploy fails without it (before it changes anything).
// The GitHub release is made on top of the tag when GitHub lets it: a
// failure there is reported in the operation and made good by the next
// deploy of the commit. Empty when this Forge API cuts no versions.
func (d *Deploys) cutVersion(ctx context.Context, owner, repo, commit, env string, bump semver.Bump, emit func(string, string)) (string, error) {
	if d.o.Releases == nil {
		return "", nil
	}
	lock := d.cutLock(owner + "/" + repo)
	lock.Lock()
	defer lock.Unlock()
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
		is := fmt.Sprintf("%s is %s", shortSHA(commit), tag)
		switch {
		case !p.tagged:
			err := d.o.Releases.CreateTag(ctx, owner, repo, tag, commit)
			if errors.Is(err, github.ErrTagExists) {
				continue // taken since the tags were read: work it out again
			}
			if err != nil {
				return "", failed("tag "+shortSHA(commit)+" as "+tag, err)
			}
		case p.version.Final() && !final:
			// Staging runs a released commit. The release of a final
			// version is made by the deploy to production, never from here.
			emit("release", is)
			return tag, nil
		default:
			ok, err := d.o.Releases.ReleaseExists(ctx, owner, repo, tag)
			if ok || err != nil {
				if err != nil {
					d.o.Log.Warn("read a release", "repo", owner+"/"+repo, "tag", tag, "err", err)
				}
				emit("release", is)
				return tag, nil
			}
		}
		rel := github.Release{Tag: tag, Commit: commit, Previous: p.previous, Prerelease: !p.version.Final(), Latest: p.latest}
		if err := d.o.Releases.CreateRelease(ctx, owner, repo, rel); err != nil {
			d.o.Log.Warn("create a release", "repo", owner+"/"+repo, "tag", tag, "err", err)
			emit("release", fmt.Sprintf("%s; its GitHub release could not be made and is tried again when this commit is deployed again: %v", is, err))
			return tag, nil
		}
		kind := "released"
		if !p.version.Final() {
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
