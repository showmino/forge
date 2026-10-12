package envtest

import (
	"context"
	"crypto/sha1" //nolint:gosec // a fake blob id, as Git's
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/showmino/forge/internal/github"
)

// DocsGitHub is GitHub for DocsService: repos with one default branch whose
// head is a set of files. Push replaces the files and moves the head.
type DocsGitHub struct {
	mu    sync.Mutex
	repos map[string]*docsRepo
	blobs map[string][]byte
	// Calls counts the calls by method name.
	Calls map[string]int
	// Fail makes every call fail, as GitHub being down.
	Fail error
}

type docsRepo struct {
	branch, head string
	files        map[string][]byte
	changed      time.Time
	n            int
}

// Push makes files the content of owner/repo's default branch at a new commit.
func (g *DocsGitHub) Push(owner, repo string, files map[string]string, at time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.repos == nil {
		g.repos, g.blobs, g.Calls = map[string]*docsRepo{}, map[string][]byte{}, map[string]int{}
	}
	key := owner + "/" + repo
	r := g.repos[key]
	if r == nil {
		r = &docsRepo{branch: "main"}
		g.repos[key] = r
	}
	r.n++
	r.head = blobSHA(fmt.Appendf(nil, "commit %s %d", key, r.n))
	r.files = map[string][]byte{}
	for p, c := range files {
		r.files[p] = []byte(c)
		g.blobs[blobSHA([]byte(c))] = []byte(c)
	}
	r.changed = at
	return r.head
}

func blobSHA(data []byte) string {
	h := sha1.Sum(data) //nolint:gosec // a fake blob id
	return hex.EncodeToString(h[:])
}

func (g *DocsGitHub) call(name, owner, repo string) (*docsRepo, error) {
	g.Calls[name]++
	if g.Fail != nil {
		return nil, g.Fail
	}
	r := g.repos[owner+"/"+repo]
	if r == nil {
		return nil, fmt.Errorf("%s/%s: %w", owner, repo, github.ErrNotFound)
	}
	return r, nil
}

func (g *DocsGitHub) ResolveRef(_ context.Context, owner, repo, ref string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.call("ResolveRef", owner, repo)
	if err != nil {
		return "", err
	}
	if ref != "HEAD" && ref != r.branch && ref != r.head {
		return "", github.ErrNotFound
	}
	return r.head, nil
}

func (g *DocsGitHub) DefaultBranch(_ context.Context, owner, repo string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.call("DefaultBranch", owner, repo)
	if err != nil {
		return "", err
	}
	return r.branch, nil
}

// Tree answers the head commit with the top level of the repo, and a
// folder's id (as that answer gives it) with everything below the folder.
func (g *DocsGitHub) Tree(_ context.Context, owner, repo, treeish string, recursive bool) (*github.Tree, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.call("Tree", owner, repo)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if treeish != r.head {
		dir, ok := strings.CutPrefix(treeish, "tree:"+r.head+":")
		if !ok {
			return nil, github.ErrNotFound
		}
		prefix = dir + "/"
	}
	t := &github.Tree{SHA: treeish}
	dirs := map[string]bool{}
	paths := make([]string, 0, len(r.files))
	for p := range r.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		if dir, _, nested := strings.Cut(rel, "/"); nested && !recursive {
			if !dirs[dir] {
				dirs[dir] = true
				t.Entries = append(t.Entries, github.TreeEntry{Path: dir, Type: "tree", SHA: "tree:" + r.head + ":" + prefix + dir})
			}
			continue
		}
		if recursive {
			parts := strings.Split(rel, "/")
			for i := 1; i < len(parts); i++ {
				d := strings.Join(parts[:i], "/")
				if !dirs[d] {
					dirs[d] = true
					t.Entries = append(t.Entries, github.TreeEntry{Path: d, Type: "tree", SHA: "tree:" + r.head + ":" + prefix + d})
				}
			}
		}
		t.Entries = append(t.Entries, github.TreeEntry{Path: rel, Type: "blob", SHA: blobSHA(r.files[p]), Size: int64(len(r.files[p]))})
	}
	return t, nil
}

func (g *DocsGitHub) Blob(_ context.Context, owner, repo, sha string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.call("Blob", owner, repo); err != nil {
		return nil, err
	}
	data, ok := g.blobs[sha]
	if !ok {
		return nil, github.ErrNotFound
	}
	return data, nil
}

// LastChange answers with the head and the time of the last Push.
func (g *DocsGitHub) LastChange(_ context.Context, owner, repo, _, _ string) (github.CommitInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.call("LastChange", owner, repo)
	if err != nil {
		return github.CommitInfo{}, err
	}
	return github.CommitInfo{SHA: r.head, Message: "docs", Time: r.changed}, nil
}
