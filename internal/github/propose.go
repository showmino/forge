package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
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
// changes nothing when a file exists already. When the branch exists with an
// open pull request (a retry), it returns that one. It returns the pull
// request's URL.
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
	git := rf.git()
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
		e := map[string]any{"path": f.Path, "mode": mode, "type": "blob"}
		if utf8.Valid(f.Content) {
			e["content"] = string(f.Content)
		} else {
			var blob struct {
				SHA string `json:"sha"`
			}
			in := map[string]string{"content": base64.StdEncoding.EncodeToString(f.Content), "encoding": "base64"}
			if err := c.doJSON(ctx, http.MethodPost, git+"/blobs", in, &blob); err != nil {
				return "", fmt.Errorf("upload %s: %w", f.Path, err)
			}
			e["sha"] = blob.SHA
		}
		entries = append(entries, e)
	}
	var newTree, commit struct {
		SHA string `json:"sha"`
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/trees", map[string]any{"base_tree": tree, "tree": entries}, &newTree); err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	if err := c.doJSON(ctx, http.MethodPost, git+"/commits", map[string]any{"message": p.Message, "tree": newTree.SHA, "parents": []string{head}}, &commit); err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	err = c.doJSON(ctx, http.MethodPost, git+"/refs", map[string]any{"ref": "refs/heads/" + p.Branch, "sha": commit.SHA}, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity && strings.Contains(se.Body, "already exists") {
		if url, ok, ferr := c.openPull(ctx, owner, repo, p.Branch); ferr == nil && ok {
			return url, nil
		}
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
		// Without its pull request the branch would only block the next try.
		if derr := c.doJSON(ctx, http.MethodDelete, git+"/refs/heads/"+escapeRef(p.Branch), nil, nil); derr != nil {
			return "", fmt.Errorf("open the pull request from %s: %w (the branch is left behind: %w)", p.Branch, err, derr)
		}
		return "", fmt.Errorf("open the pull request from %s: %w", p.Branch, err)
	}
	return pr.HTMLURL, nil
}

// openPull returns the URL of the open pull request from branch, if there is one.
func (c *Client) openPull(ctx context.Context, owner, repo, branch string) (string, bool, error) {
	var pulls []struct {
		HTMLURL string `json:"html_url"`
	}
	q := "?state=open&head=" + url.QueryEscape(owner+":"+branch)
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/pulls"+q, nil, &pulls); err != nil {
		return "", false, err
	}
	if len(pulls) == 0 {
		return "", false, nil
	}
	return pulls[0].HTMLURL, true, nil
}
