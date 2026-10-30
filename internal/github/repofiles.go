package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// ErrNotFastForward is returned when a branch moved since the commit's parent
// was read; the caller rebuilds on the new head.
var ErrNotFastForward = errors.New("the branch moved")

// CommitInfo is one commit of a file's history.
type CommitInfo struct {
	SHA, Message string
	Time         time.Time
}

// RepoFiles reads and commits single files on one branch of one repo through
// the git data API. Forge's deploy repo is written this way, one file per commit.
type RepoFiles struct {
	Client              *Client
	Owner, Name, Branch string
}

func (r *RepoFiles) git() string { return repoPath(r.Owner, r.Name) + "/git" }

// Read returns path at ref (default: the branch). A missing file is ErrNotFound.
func (r *RepoFiles) Read(ctx context.Context, path, ref string) ([]byte, error) {
	if ref == "" {
		ref = r.Branch
	}
	return r.Client.FileAt(ctx, r.Owner, r.Name, path, ref)
}

// Head returns the branch's commit and that commit's tree.
func (r *RepoFiles) Head(ctx context.Context) (string, string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := r.Client.doJSON(ctx, http.MethodGet, r.git()+"/ref/heads/"+escapeRef(r.Branch), nil, &ref); err != nil {
		return "", "", err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := r.Client.doJSON(ctx, http.MethodGet, r.git()+"/commits/"+ref.Object.SHA, nil, &commit); err != nil {
		return "", "", err
	}
	return ref.Object.SHA, commit.Tree.SHA, nil
}

// CommitFile commits path=content on top of parent (whose tree is tree) and
// moves the branch to it without forcing. If the branch is no longer at
// parent, it returns ErrNotFastForward and the branch is unchanged.
func (r *RepoFiles) CommitFile(ctx context.Context, parent, tree, path string, content []byte, message string) (string, error) {
	return r.CommitFiles(ctx, parent, tree, map[string][]byte{path: content}, message)
}

// DeleteFile commits the removal of path on top of parent (whose tree is
// tree) and moves the branch to it without forcing, as CommitFile does. The
// path must exist in tree.
func (r *RepoFiles) DeleteFile(ctx context.Context, parent, tree, path, message string) (string, error) {
	return r.CommitFiles(ctx, parent, tree, map[string][]byte{path: nil}, message)
}

// CommitFiles commits every path=content of files in one commit on top of
// parent, as CommitFile does; a nil content removes the path, which must
// then exist in tree.
func (r *RepoFiles) CommitFiles(ctx context.Context, parent, tree string, files map[string][]byte, message string) (string, error) {
	var newTree, commit struct {
		SHA string `json:"sha"`
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	entries := make([]map[string]any, 0, len(files))
	for _, p := range paths {
		if c := files[p]; c != nil {
			entries = append(entries, map[string]any{"path": p, "mode": "100644", "type": "blob", "content": string(c)})
		} else {
			// A tree entry with a null sha removes the path from the base tree.
			entries = append(entries, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": nil})
		}
	}
	in := map[string]any{"base_tree": tree, "tree": entries}
	if err := r.Client.doJSON(ctx, http.MethodPost, r.git()+"/trees", in, &newTree); err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	if err := r.Client.doJSON(ctx, http.MethodPost, r.git()+"/commits",
		map[string]any{"message": message, "tree": newTree.SHA, "parents": []string{parent}}, &commit); err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	err := r.Client.doJSON(ctx, http.MethodPatch, r.git()+"/refs/heads/"+escapeRef(r.Branch), map[string]any{"sha": commit.SHA, "force": false}, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity {
		return "", fmt.Errorf("%w: %w", ErrNotFastForward, err)
	}
	if err != nil {
		return "", fmt.Errorf("update %s: %w", r.Branch, err)
	}
	return commit.SHA, nil
}

// History returns up to limit commits that changed path, newest first.
func (r *RepoFiles) History(ctx context.Context, path string, limit int) ([]CommitInfo, error) {
	var out []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	q := url.Values{"path": {path}, "sha": {r.Branch}, "per_page": {strconv.Itoa(limit)}}
	if err := r.Client.doJSON(ctx, http.MethodGet, repoPath(r.Owner, r.Name)+"/commits?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	hist := make([]CommitInfo, 0, len(out))
	for _, c := range out {
		hist = append(hist, CommitInfo{SHA: c.SHA, Message: c.Commit.Message, Time: c.Commit.Committer.Date})
	}
	return hist, nil
}
