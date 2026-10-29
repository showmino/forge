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
	// stagingInternal is run.staging.public: false.
	stagingInternal bool
	// run is forge.yaml's run section, nil without one.
	run *manifest.Run
	// imageOnly is set for a service without a repo: the image its
	// forge.yaml in the deploy repo names, in full. Then commit is the
	// image's digest, ref its tag and image the image pinned to the digest.
	imageOnly string
}

// resolveSource checks service ("services/<id>") and ref (default main),
// resolves the ref to a commit and reads forge.yaml's run settings there.
// registry is where the services' CI pushes images ("" is ghcr.io).
func resolveSource(ctx context.Context, cat *Catalog, gh EnvGitHub, org, registry, service, ref string) (source, error) {
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
	if svc.GetRepoUrl() == "" {
		return s, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s%s has no repo: it runs an image, and this needs a branch or a commit of a repo", servicePrefix, sid))
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
		if mf.RunImage() != "" {
			return s, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"forge.yaml at %s has run.image, which is for a service without a repo; %s%s runs what its CI builds", shortSHA(s.commit), servicePrefix, sid))
		}
		s.port, s.health = int32(mf.RunPort()), mf.RunHealth() //nolint:gosec // Parse checks run.port is 1-65535
		s.domain, s.host = mf.RunDomain(), mf.RunHost()
		s.stagingInternal = mf.StagingInternal()
		s.run = mf.Run
	}
	s.image = imageName(registry, s.owner, s.serviceID, s.commit)
	return s, nil
}

// DefaultRegistry is where images are when the platform names no registry.
const DefaultRegistry = "ghcr.io"

// imageName is the image the services' CI pushes for a commit:
// <registry>/<owner, lower case>/<service>:<short commit>.
func imageName(registry, owner, service, commit string) string {
	if registry == "" {
		registry = DefaultRegistry
	}
	return registry + "/" + strings.ToLower(owner) + "/" + service + ":" + shortSHA(commit)
}

// waitForImage polls the registry until the image exists, failing fast when
// a CI run for the commit finished without success, or when every run has
// been finished, with no image, for a tenth of the timeout (and two looks).
func waitForImage(ctx context.Context, gh EnvGitHub, images ImageChecker, s source, timeout, poll time.Duration, emit func(step, message string)) error {
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	last := ""
	// The grace covers runs GitHub creates late, such as a workflow_run build.
	grace := max(timeout/10, 2*poll)
	var finishedSince time.Time // zero unless every CI run is done and the image is missing
	looks := 0
	for {
		ok, err := images.Exists(ctx, s.image)
		if err == nil && ok {
			emit("image", "found "+s.image)
			return nil
		}
		status := "unknown"
		runsDone := false
		if runs, rerr := gh.RunsFor(ctx, s.owner, s.repo, s.commit); rerr == nil {
			if r := failedRun(runs); r != nil {
				return &OpError{Code: "ci_failed", Message: fmt.Sprintf("CI %q for %s finished with %s: %s", r.Name, shortSHA(s.commit), r.Conclusion, r.HTMLURL)}
			}
			status = "not started"
			if len(runs) > 0 {
				status = runs[0].Status
			}
			runsDone = err == nil && allCompleted(runs)
		}
		if !runsDone {
			finishedSince, looks = time.Time{}, 0
		} else {
			if finishedSince.IsZero() {
				finishedSince = time.Now()
			}
			if looks++; looks >= 2 && time.Since(finishedSince) >= grace {
				return &OpError{Code: "no_image", Message: fmt.Sprintf("every CI run for %s has finished and %s is not in the registry. Likely: the workflow builds no image for this commit (in a service made from an older template, the image job of .github/workflows/ci.yml runs only on main), the image was deleted, or it is pushed under another name", shortSHA(s.commit), s.image)}
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

// allCompleted reports whether there are runs and every one has finished.
func allCompleted(runs []github.WorkflowRun) bool {
	for _, r := range runs {
		if r.Status != "completed" {
			return false
		}
	}
	return len(runs) > 0
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
