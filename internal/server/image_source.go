package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
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

// ImageUsers finds the user an image runs as (its config's User). When
// DeployOptions.Digests implements it, as *registry.Client does, an image
// that would run as root is refused before it is written or deployed.
type ImageUsers interface {
	User(ctx context.Context, image string) (string, error)
}

// checkUser refuses an image whose user the service chart cannot run:
// every service runs with runAsNonRoot, which needs a user given by a
// number other than 0.
func checkUser(image, user string) error {
	const advice = "Forge runs every service as a non-root user: use an image built to run as one, with a numeric USER " +
		"(such as nginxinc/nginx-unprivileged instead of nginx), or build one"
	uid, _, _ := strings.Cut(user, ":")
	if uid == "" || uid == "0" || uid == "root" {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s runs as root (its image sets no non-root USER). %s", image, advice))
	}
	if n, err := strconv.ParseUint(uid, 10, 32); err != nil || n == 0 {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s runs as the user %q, a name, and Kubernetes can only check that a user given by number is not root. %s", image, user, advice))
	}
	return nil
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
			AllowedRegistries: d.o.AllowedRegistries, ServiceImages: d.serviceImages(),
			Actor: policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)},
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
	p := pinnedImage{full: full, ref: r, digest: digest, pinned: r.Pinned(digest)}
	if users, ok := d.o.Digests.(ImageUsers); ok {
		user, err := users.User(ctx, p.pinned)
		if errors.Is(err, registry.ErrBadImage) {
			return pinnedImage{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("read the user %s runs as: %w", full, err))
		}
		if err != nil {
			return pinnedImage{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the user %s runs as: %w", full, err))
		}
		if err := checkUser(full, user); err != nil {
			return pinnedImage{}, err
		}
	}
	return p, nil
}

// serviceImages is where the platform's services' CI pushes their images:
// "<registry>/<org>/", as imageName names them.
func (d *Deploys) serviceImages() string {
	if d.o.Org == "" {
		return ""
	}
	registry := d.o.Registry
	if registry == "" {
		registry = DefaultRegistry
	}
	return strings.ToLower(registry) + "/" + strings.ToLower(d.o.Org) + "/"
}

// imageSource is what a deploy of the service sid, which has no repo, runs:
// the image its forge.yaml in the deploy repo names, or that image at the
// tag ref when one is given. retag reports that ref changes the image: once
// the deploy has succeeded, the tag is written into forge.yaml, so that the
// next deploy runs it too.
func (d *Deploys) imageSource(ctx context.Context, sid, ref string) (src source, retag bool, err error) {
	data, err := d.o.Repo.Read(ctx, manifestPath(sid), "")
	if errors.Is(err, github.ErrNotFound) {
		return src, false, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s%s has no repo and no %s in the deploy repo: make a service that runs an image with forge create --image", servicePrefix, sid, manifestPath(sid)))
	}
	if err != nil {
		return src, false, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the deploy repo: %w", err))
	}
	mf, err := manifest.Parse(data)
	if err != nil {
		return src, false, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s in the deploy repo is invalid: %w", manifestPath(sid), err))
	}
	image := mf.RunImage()
	if image == "" {
		return src, false, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s in the deploy repo names no run.image", manifestPath(sid)))
	}
	if ref != "" {
		r, err := registry.ParseRef(image)
		if err != nil {
			return src, false, connect.NewError(connect.CodeInternal, err)
		}
		image = r.Host + "/" + r.Repo + ":" + ref
	}
	p, err := d.pinImage(ctx, sid, image)
	if err != nil {
		return src, false, err
	}
	retag = p.full != mf.RunImage()
	src = source{
		serviceID: sid, ref: p.version(), commit: p.digest, image: p.pinned,
		port: int32(mf.RunPort()), health: mf.RunHealth(), //nolint:gosec // Parse checks run.port is 1-65535
		domain: mf.RunDomain(), host: mf.RunHost(), stagingInternal: mf.StagingInternal(), run: mf.Run,
		imageOnly: p.full,
	}
	return src, retag, nil
}

// resolve is what a deploy of service at ref runs: from its repo, or for a
// service without one, the image its forge.yaml in the deploy repo names.
func (d *Deploys) resolve(ctx context.Context, service, ref string) (source, bool, error) {
	sid, ok := strings.CutPrefix(service, servicePrefix)
	if !ok || sid == "" {
		return source{}, false, invalidArg("service %q must be services/<name>", service)
	}
	repoless, err := d.repoless(ctx, sid)
	if err != nil {
		return source{}, false, err
	}
	if repoless {
		return d.imageSource(ctx, sid, ref)
	}
	src, err := resolveSource(ctx, d.o.Catalog, d.o.GitHub, d.o.Org, d.o.Registry, service, ref)
	return src, false, err
}

// repoless reports whether the service sid has no repo: it runs an image.
func (d *Deploys) repoless(ctx context.Context, sid string) (bool, error) {
	entry, err := d.o.Catalog.get(ctx, servicePrefix+sid)
	if err != nil {
		return false, err // NotFound "services/<id> not found", or Unavailable
	}
	return entry.GetRepoUrl() == "", nil
}

// retagManifest writes image into the forge.yaml of sid as it is at the
// deploy repo's head, keeping the rest of it.
func (d *Deploys) retagManifest(ctx context.Context, sid, image, message string, emit func(string, string)) error {
	build := func() ([]byte, error) {
		data, err := d.o.Repo.Read(ctx, manifestPath(sid), "")
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", manifestPath(sid), err)
		}
		return manifest.SetRunImage(data, image)
	}
	sha, err := d.commit(ctx, manifestPath(sid), build, message, emit)
	if err != nil {
		return err
	}
	emit("manifest", fmt.Sprintf("forge-deploys %s: run.image is %s", shortSHA(sha), image))
	return nil
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
	return d.removeManifestIf(ctx, sid, nil, emit)
}

// removeManifestIf is removeManifest, but when only is set it removes the
// file only while it still holds only: a failed create takes back its own
// write and nothing someone wrote since.
func (d *Deploys) removeManifestIf(ctx context.Context, sid string, only []byte, emit func(string, string)) error {
	path := manifestPath(sid)
	for range 3 {
		head, tree, err := d.o.Repo.Head(ctx)
		if err != nil {
			return fmt.Errorf("deploy repo head: %w", err)
		}
		got, err := d.o.Repo.Read(ctx, path, head)
		if errors.Is(err, github.ErrNotFound) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read %s in the deploy repo: %w", path, err)
		}
		if only != nil && !bytes.Equal(got, only) {
			return nil // someone else's by now
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
