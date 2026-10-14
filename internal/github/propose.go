package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ErrFileExists means a proposed file is on the default branch already.
var ErrFileExists = errors.New("the file already exists")

// ErrBranchExists means the proposal's branch exists already.
var ErrBranchExists = errors.New("the branch already exists")

// Proposal is a pull request that adds files to a repo.
type Proposal struct {
	Branch, Title, Body, Message string
	Files                        []File
}

// ProposeFiles commits p's files on top of the default branch to a new
// branch p.Branch and opens a pull request into the default branch. It
// changes nothing when a file or the branch exists already. It returns the
// pull request's URL.
func (c *Client) ProposeFiles(ctx context.Context, owner, repo string, p Proposal) (string, error) {
	base, err := c.DefaultBranch(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	rf := &RepoFiles{Client: c, Owner: owner, Name: repo, Branch: base}
	head, tree, err := rf.Head(ctx)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", base, err)
	}
	entries := make([]map[string]any, 0, len(p.Files))
	for _, f := range p.Files {
		_, err := c.FileAt(ctx, owner, repo, f.Path, head)
		if err == nil {
			return "", fmt.Errorf("%s in %s/%s: %w", f.Path, owner, repo, ErrFileExists)
		}
		if !errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("check %s: %w", f.Path, err)
		}
		mode := "100644"
		if f.Executable {
			mode = "100755"
		}
		entries = append(entries, map[string]any{"path": f.Path, "mode": mode, "type": "blob", "content": string(f.Content)})
	}
	var newTree, commit struct {
		SHA string `json:"sha"`
	}
	git := rf.git()
	if err := c.doJSON(ctx, http.MethodPost, git+"/trees", map[string]any{"base_tree": tree, "tree": entries}, &newTree); err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/commits", map[string]any{"message": p.Message, "tree": newTree.SHA, "parents": []string{head}}, &commit); err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	err = c.doJSON(ctx, http.MethodPost, git+"/refs", map[string]any{"ref": "refs/heads/" + p.Branch, "sha": commit.SHA}, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity {
		return "", fmt.Errorf("%s in %s/%s: %w", p.Branch, owner, repo, ErrBranchExists)
	}
	if err != nil {
		return "", fmt.Errorf("create %s: %w", p.Branch, err)
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	in := map[string]any{"title": p.Title, "body": p.Body, "head": p.Branch, "base": base}
	if err := c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/pulls", in, &pr); err != nil {
		return "", fmt.Errorf("open the pull request from %s: %w", p.Branch, err)
	}
	return pr.HTMLURL, nil
}
