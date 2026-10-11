package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// source is a service's code at one commit: where it lives and how it runs.
type source struct {
	serviceID, owner, repo string
	ref, commit, image     string
	port                   int32
	health                 string
	// domain and host are run.domain and run.host as forge.yaml has them:
	// "" for the platform's default domain and for the service's own name.
	domain, host string
}

// resolveSource checks service ("services/<id>") and ref (default main),
// resolves the ref to a commit and reads forge.yaml's run settings there.
func resolveSource(ctx context.Context, cat *Catalog, gh EnvGitHub, org, service, ref string) (source, error) {
	s := source{ref: ref}
	if s.ref == "" {
		s.ref = "main"
	}
	if !refRE.MatchString(s.ref) || strings.Contains(s.ref, "..") {
		return s, invalidArg("%q is not a valid Git ref", s.ref)
	}
	sid, ok := strings.CutPrefix(service, servicePrefix)
	if !ok || sid == "" {
		return s, invalidArg("service %q must be services/<name>", service)
	}
	s.serviceID = sid
	svc, err := cat.get(ctx, servicePrefix+sid)
	if err != nil {
		return s, err // NotFound "services/<id> not found", or Unavailable
	}
	if s.owner, s.repo, ok = repoOf(svc.GetRepoUrl(), org); !ok {
		return s, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s%s has no GitHub repo in %s (repo_url %q)", servicePrefix, sid, org, svc.GetRepoUrl()))
	}
	s.commit, err = gh.ResolveRef(ctx, s.owner, s.repo, s.ref)
	if errors.Is(err, github.ErrNotFound) {
		return s, connect.NewError(connect.CodeNotFound, fmt.Errorf("ref %q not found in github.com/%s/%s", s.ref, s.owner, s.repo))
	}
	if err != nil {
		return s, connect.NewError(connect.CodeUnavailable, fmt.Errorf("resolve %s in github.com/%s/%s: %w", s.ref, s.owner, s.repo, err))
	}
	s.port, s.health = manifest.DefaultRunPort, manifest.DefaultRunHealth
	data, err := gh.FileAt(ctx, s.owner, s.repo, manifest.FileName, s.commit)
	switch {
	case errors.Is(err, github.ErrNotFound): // no forge.yaml: defaults
	case err != nil:
		return s, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read forge.yaml at %s: %w", shortSHA(s.commit), err))
	default:
		mf, err := manifest.Parse(data)
		if err != nil {
			return s, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("forge.yaml at %s is invalid: %w", shortSHA(s.commit), err))
		}
		s.port, s.health = int32(mf.RunPort()), mf.RunHealth() //nolint:gosec // Parse checks run.port is 1-65535
		s.domain, s.host = mf.RunDomain(), mf.RunHost()
	}
	s.image = "ghcr.io/" + strings.ToLower(s.owner) + "/" + s.serviceID + ":" + shortSHA(s.commit)
	return s, nil
}

// waitForImage polls the registry until the image exists, failing fast when
// a CI run for the commit finished without success.
func waitForImage(ctx context.Context, gh EnvGitHub, images ImageChecker, s source, timeout, poll time.Duration, emit func(step, message string)) error {
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	last := ""
	for {
		ok, err := images.Exists(ctx, s.image)
		if err == nil && ok {
			emit("image", "found "+s.image)
			return nil
		}
		status := "unknown"
		if runs, rerr := gh.RunsFor(ctx, s.owner, s.repo, s.commit); rerr == nil {
			if r := failedRun(runs); r != nil {
				return &OpError{Code: "ci_failed", Message: fmt.Sprintf("CI %q for %s finished with %s: %s", r.Name, shortSHA(s.commit), r.Conclusion, r.HTMLURL)}
			}
			status = "not started"
			if len(runs) > 0 {
				status = runs[0].Status
			}
		}
		msg := fmt.Sprintf("waiting for %s (CI: %s)", s.image, status)
		if err != nil {
			msg += "; registry: " + clip(err.Error())
		}
		if msg != last {
			emit("image", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tm.C:
			return &OpError{Code: "image_timeout", Message: fmt.Sprintf("%s did not appear within %s", s.image, timeout)}
		case <-time.After(poll):
		}
	}
}

// failedRun returns the newest run of a workflow when it finished without
// success; older runs of the same workflow do not count.
func failedRun(runs []github.WorkflowRun) *github.WorkflowRun {
	seen := map[string]bool{}
	for i := range runs {
		r := &runs[i]
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		if r.Status == "completed" && r.Conclusion != "success" && r.Conclusion != "skipped" && r.Conclusion != "neutral" {
			return r
		}
	}
	return nil
}
