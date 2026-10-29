package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/registry"
	"github.com/showmino/forge/policy"
)

// A service without a repo runs an image someone else built (phase 9). Its
// forge.yaml, with run.image, lives in the deploy repo next to its
// environments' values; a deploy pins the image's tag to its digest, so
// promotion moves exactly what staging ran.

// ImageDigester finds the digest of an image. *registry.Client implements it.
type ImageDigester interface {
	Digest(ctx context.Context, image string) (string, error)
}

// manifestPath is where the forge.yaml of a service without a repo lives in
// the deploy repo.
func manifestPath(svc string) string { return "services/" + svc + "/" + manifest.FileName }

// pinnedImage is an image checked against the image rules, with its digest.
type pinnedImage struct {
	// full is the image as forge.yaml writes it: docker.io/library/nginx:1.28.
	full string
	ref  registry.Ref
	// digest is sha256:..., and pinned the image at it, which the cluster pulls.
	digest, pinned string
}

// version is what a deploy of the image runs as: its tag, or for an image
// named by digest alone the start of the digest.
func (p pinnedImage) version() string {
	if p.ref.Tag != "" {
		return p.ref.Tag
	}
	return shortDigest(p.digest)
}

func shortDigest(d string) string {
	if h, ok := strings.CutPrefix(d, "sha256:"); ok && len(h) >= 12 {
		return "sha256:" + h[:12]
	}
	return d
}

// pinImage checks image for the service svc against the image rules and
// finds its digest.
func (d *Deploys) pinImage(ctx context.Context, svc, image string) (pinnedImage, error) {
	full, err := registry.Normalize(image)
	if err != nil {
		return pinnedImage{}, invalidArg("%v", err)
	}
	r, err := registry.ParseRef(full)
	if err != nil {
		return pinnedImage{}, invalidArg("%v", err)
	}
	if d.o.Policy != nil {
		res, err := d.o.Policy.EvaluateImage(ctx, policy.Image{
			Service: svc, Image: full, Host: r.Host, Repo: r.Repo, Tag: r.Tag, Digest: r.Digest,
			AllowedRegistries: d.o.AllowedRegistries,
			Actor:             policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)},
		})
		if err != nil {
			return pinnedImage{}, connect.NewError(connect.CodeInternal, fmt.Errorf("check the image rules: %w", err))
		}
		noteAuditDetail(ctx, "policy_rules", d.o.Policy.Fingerprint())
		if res.Denied() {
			return pinnedImage{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the image rules refuse it: %s", strings.Join(res.Deny, "; ")))
		}
	}
	if d.o.Digests == nil {
		return pinnedImage{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("this Forge API cannot look up images in registries, so it cannot run one without a repo"))
	}
	digest, err := d.o.Digests.Digest(ctx, full)
	if errors.Is(err, registry.ErrNotFound) {
		return pinnedImage{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is not in its registry: check the name and the tag", full))
	}
	if err != nil {
		return pinnedImage{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("look up %s: %w", full, err))
	}
	if r.Digest != "" && r.Digest != digest {
		return pinnedImage{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: the registry has %s for it, not the digest it names", full, digest))
	}
	return pinnedImage{full: full, ref: r, digest: digest, pinned: r.Pinned(digest)}, nil
}

// imageSource is what a deploy of the service sid, which has no repo, runs:
// the image its forge.yaml in the deploy repo names, or that image at the
// tag ref when one is given. When ref changes the image, newManifest is the
// forge.yaml to write, so that the next deploy runs the same tag.
func (d *Deploys) imageSource(ctx context.Context, sid, ref string) (src source, newManifest []byte, err error) {
	data, err := d.o.Repo.Read(ctx, manifestPath(sid), "")
	if errors.Is(err, github.ErrNotFound) {
		return src, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s%s has no repo and no %s in the deploy repo: make a service that runs an image with forge create --image", servicePrefix, sid, manifestPath(sid)))
	}
	if err != nil {
		return src, nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the deploy repo: %w", err))
	}
	mf, err := manifest.Parse(data)
	if err != nil {
		return src, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s in the deploy repo is invalid: %w", manifestPath(sid), err))
	}
	image := mf.RunImage()
	if image == "" {
		return src, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s in the deploy repo names no run.image", manifestPath(sid)))
	}
	if ref != "" {
		r, err := registry.ParseRef(image)
		if err != nil {
			return src, nil, connect.NewError(connect.CodeInternal, err)
		}
		image = r.Host + "/" + r.Repo + ":" + ref
	}
	p, err := d.pinImage(ctx, sid, image)
	if err != nil {
		return src, nil, err
	}
	if p.full != mf.RunImage() {
		if newManifest, err = manifest.SetRunImage(data, p.full); err != nil {
			return src, nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	src = source{
		serviceID: sid, ref: p.version(), commit: p.digest, image: p.pinned,
		port: int32(mf.RunPort()), health: mf.RunHealth(), //nolint:gosec // Parse checks run.port is 1-65535
		domain: mf.RunDomain(), host: mf.RunHost(), stagingInternal: mf.StagingInternal(), run: mf.Run,
		imageOnly: p.full,
	}
	return src, newManifest, nil
}

// resolve is what a deploy of service at ref runs: from its repo, or for a
// service without one, the image its forge.yaml in the deploy repo names.
func (d *Deploys) resolve(ctx context.Context, service, ref string) (source, []byte, error) {
	sid, ok := strings.CutPrefix(service, servicePrefix)
	if !ok || sid == "" {
		return source{}, nil, invalidArg("service %q must be services/<name>", service)
	}
	repoless, err := d.repoless(ctx, sid)
	if err != nil {
		return source{}, nil, err
	}
	if repoless {
		return d.imageSource(ctx, sid, ref)
	}
	src, err := resolveSource(ctx, d.o.Catalog, d.o.GitHub, d.o.Org, d.o.Registry, service, ref)
	return src, nil, err
}

// repoless reports whether the service sid has no repo: it runs an image.
func (d *Deploys) repoless(ctx context.Context, sid string) (bool, error) {
	entry, err := d.o.Catalog.get(ctx, servicePrefix+sid)
	if err != nil {
		return false, err // NotFound "services/<id> not found", or Unavailable
	}
	return entry.GetRepoUrl() == "", nil
}

// writeManifest commits the forge.yaml of a service without a repo.
func (d *Deploys) writeManifest(ctx context.Context, sid string, data []byte, message string, emit func(string, string)) error {
	sha, err := d.commit(ctx, manifestPath(sid), func() ([]byte, error) { return data, nil }, message, emit)
	if err != nil {
		return err
	}
	emit("manifest", fmt.Sprintf("forge-deploys %s: %s", shortSHA(sha), manifestPath(sid)))
	return nil
}

// removeManifest deletes the forge.yaml of a service without a repo from
// the deploy repo, when it is there. Safe to repeat.
func (d *Deploys) removeManifest(ctx context.Context, sid string, emit func(string, string)) error {
	path := manifestPath(sid)
	for range 3 {
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return fmt.Errorf("deploy repo head: %w", err)
		}
		if _, err := d.o.Repo.Read(ctx, path, head); errors.Is(err, github.ErrNotFound) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read %s in the deploy repo: %w", path, err)
		}
		sha, err := d.o.Repo.DeleteFile(ctx, head, tree, path, fmt.Sprintf("remove the forge.yaml of %s (%s)", sid, actorOf(ctx)))
		if errors.Is(err, github.ErrNotFastForward) {
			continue
		}
		if err != nil {
			return fmt.Errorf("remove %s from the deploy repo: %w", path, err)
		}
		emit("manifest", fmt.Sprintf("forge-deploys %s: removed %s", shortSHA(sha), path))
		return nil
	}
	return &OpError{Code: "git_conflict", Message: "the deploy repo kept moving; delete again"}
}

// hasManifest reports whether the deploy repo holds a forge.yaml for sid.
func (d *Deploys) hasManifest(ctx context.Context, sid string) (bool, error) {
	_, err := d.o.Repo.Read(ctx, manifestPath(sid), "")
	if errors.Is(err, github.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
