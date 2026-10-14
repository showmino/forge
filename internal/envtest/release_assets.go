package envtest

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/showmino/forge/internal/github"
)

// ReleaseAssets is an in-memory set of GitHub release assets, keyed
// "owner/repo/tag/name" (server.CLIReleases).
type ReleaseAssets map[string][]byte

// ReleaseAsset returns the asset, or github.ErrNotFound.
func (r ReleaseAssets) ReleaseAsset(_ context.Context, owner, repo, tag, name string) (io.ReadCloser, error) {
	data, ok := r[owner+"/"+repo+"/"+tag+"/"+name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, github.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
