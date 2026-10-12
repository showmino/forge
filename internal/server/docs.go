package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/docs"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
)

// DocsGitHub is what DocsService reads from GitHub. *github.Client has it.
type DocsGitHub interface {
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	DefaultBranch(ctx context.Context, owner, name string) (string, error)
	Tree(ctx context.Context, owner, repo, treeish string, recursive bool) (*github.Tree, error)
	Blob(ctx context.Context, owner, repo, sha string) ([]byte, error)
	LastChange(ctx context.Context, owner, repo, ref, path string) (github.CommitInfo, error)
}

const (
	// docsFresh is how long a service's docs are served before GitHub is
	// asked whether its default branch moved.
	docsFresh = time.Minute
	// docsMaxSetBytes caps the pages of one service, all kept in memory.
	docsMaxSetBytes = 32 << 20
	// docsMaxAssetCache caps the images kept in memory, across services.
	docsMaxAssetCache = 64 << 20
	// docsReads is how many files of one repo are read from GitHub at once.
	docsReads = 8
	// docsRefreshTimeout bounds a refresh that no request waits for.
	docsRefreshTimeout = 30 * time.Second
)

// docSet is one service's docs as read at one commit. none is set when the
// repo has no docs, with the reason.
type docSet struct {
	repoURL, owner, repo string
	branch, commit       string
	change               github.CommitInfo
	pages                []docs.Page
	content              map[string]string
	files                map[string]github.TreeEntry
	none                 string
}

// docsEntry is the cache of one service. mu lets one refresh run at a time;
// snap is read without it, so a request never waits for another's refresh.
type docsEntry struct {
	mu         sync.Mutex
	snap       atomic.Pointer[docsSnap]
	refreshing atomic.Bool
}

type docsSnap struct {
	set     *docSet
	checked time.Time
}

type docsService struct {
	gh      DocsGitHub
	catalog *Catalog
	org     string
	log     *slog.Logger
	now     func() time.Time
	// background runs a refresh no request waits for; tests run it inline.
	background func(func())

	mu         sync.Mutex
	entries    map[string]*docsEntry
	assets     map[string][]byte
	assetBytes int
}

func newDocsService(gh DocsGitHub, catalog *Catalog, org string, log *slog.Logger) *docsService {
	return &docsService{
		gh: gh, catalog: catalog, org: org, log: log, now: time.Now,
		background: func(f func()) { go f() },
		entries:    map[string]*docsEntry{},
		assets:     map[string][]byte{},
	}
}

var errNoDocsGitHub = connect.NewError(connect.CodeFailedPrecondition, errors.New("docs are read from GitHub: this Forge has no GitHub App configured"))

func (s *docsService) entry(name string) *docsEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[name]
	if e == nil {
		e = &docsEntry{}
		s.entries[name] = e
	}
	return e
}

// fresh returns the cached docs of svc when they need no check with GitHub.
func (s *docsService) fresh(svc *forgev1.Service) (*docsSnap, bool) {
	snap := s.entry(svc.GetName()).snap.Load()
	if snap == nil || snap.set.repoURL != svc.GetRepoUrl() {
		return nil, false
	}
	return snap, s.now().Sub(snap.checked) < docsFresh
}

// load returns svc's docs, asking GitHub whether the default branch moved
// at most once a minute, and reading the files again only when it did. When
// GitHub cannot be asked, the docs read before are served.
func (s *docsService) load(ctx context.Context, svc *forgev1.Service) (*docSet, error) {
	if snap, ok := s.fresh(svc); ok {
		return snap.set, nil
	}
	e := s.entry(svc.GetName())
	e.mu.Lock()
	defer e.mu.Unlock()
	if snap, ok := s.fresh(svc); ok {
		return snap.set, nil
	}
	old := e.snap.Load()
	if old != nil && old.set.repoURL != svc.GetRepoUrl() {
		old = nil
	}
	set, err := s.read(ctx, svc, old)
	if err != nil {
		if old != nil {
			s.log.WarnContext(ctx, "docs: serving the docs read before", "service", svc.GetName(), "error", err)
			return old.set, nil
		}
		return nil, err
	}
	e.snap.Store(&docsSnap{set: set, checked: s.now()})
	return set, nil
}

// read reads svc's docs at the head of its repo's default branch. old is
// what was read before: when the head has not moved it is kept as it is.
func (s *docsService) read(ctx context.Context, svc *forgev1.Service, old *docsSnap) (*docSet, error) {
	none := func(why string) *docSet { return &docSet{repoURL: svc.GetRepoUrl(), none: why} }
	owner, repo, ok := repoOf(svc.GetRepoUrl(), s.org)
	if !ok {
		return none("its repo is not a repo of the " + s.org + " organization on GitHub"), nil
	}
	head, err := s.gh.ResolveRef(ctx, owner, repo, "HEAD")
	if errors.Is(err, github.ErrNotFound) {
		return none("its repo cannot be read, or has no commits"), nil
	}
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	if old != nil && old.set.commit == head {
		return old.set, nil
	}
	set := &docSet{repoURL: svc.GetRepoUrl(), owner: owner, repo: repo, commit: head}
	root, err := s.gh.Tree(ctx, owner, repo, head, false)
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	var docsTree, forgeYAML string
	for _, en := range root.Entries {
		switch {
		case en.Path == docs.Dir && en.Type == "tree":
			docsTree = en.SHA
		case en.Path == manifest.FileName && en.Type == "blob":
			forgeYAML = en.SHA
		}
	}
	if docsTree == "" {
		set.none = "its repo has no " + docs.Dir + "/ folder"
		return set, nil
	}
	tree, err := s.gh.Tree(ctx, owner, repo, docsTree, true)
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	if tree.Truncated {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/ in %s/%s has more files than GitHub lists at once", docs.Dir, owner, repo))
	}
	set.files = map[string]github.TreeEntry{}
	var pages []string
	total := int64(0)
	for _, en := range tree.Entries {
		if en.Type != "blob" {
			continue
		}
		if _, err := docs.CleanPath(en.Path); err != nil {
			continue
		}
		set.files[en.Path] = en
		if docs.IsPage(en.Path) && en.Size <= docs.MaxPageBytes {
			pages = append(pages, en.Path)
			total += en.Size
		}
	}
	if _, ok := set.files[docs.Start]; !ok {
		set.none = "its repo has no " + docs.Dir + "/" + docs.Start
		return set, nil
	}
	if len(pages) > docs.MaxPages {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/ in %s/%s has %d pages; at most %d are read", docs.Dir, owner, repo, len(pages), docs.MaxPages))
	}
	if total > docsMaxSetBytes {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the pages in %s/ of %s/%s are %d bytes together; at most %d are read", docs.Dir, owner, repo, total, docsMaxSetBytes))
	}
	if set.content, err = s.readPages(ctx, owner, repo, set.files, pages); err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}

	var list []docs.Entry
	if forgeYAML != "" {
		data, err := s.gh.Blob(ctx, owner, repo, forgeYAML)
		if err != nil {
			return nil, s.unavailable(ctx, svc, err)
		}
		// A forge.yaml that does not parse leaves the default order: the docs are still worth reading.
		if m, err := manifest.Parse(data); err == nil {
			list = m.DocEntries()
		} else {
			s.log.WarnContext(ctx, "docs: forge.yaml does not parse, using the default page order", "service", svc.GetName(), "commit", head, "error", err)
		}
	}
	set.pages = docs.Order(pages, list, func(p string) string { return docs.Title([]byte(set.content[p]), p) })

	if set.branch, err = s.gh.DefaultBranch(ctx, owner, repo); err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	if set.change, err = s.gh.LastChange(ctx, owner, repo, head, docs.Dir); err != nil && !errors.Is(err, github.ErrNotFound) {
		return nil, s.unavailable(ctx, svc, err)
	}
	return set, nil
}

// readPages reads the pages' blobs, a few at a time.
func (s *docsService) readPages(ctx context.Context, owner, repo string, files map[string]github.TreeEntry, pages []string) (map[string]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		out   = make(map[string]string, len(pages))
		first error
		wg    sync.WaitGroup
		slots = make(chan struct{}, docsReads)
	)
	for _, p := range pages {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			data, err := s.gh.Blob(ctx, owner, repo, files[p].SHA)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = fmt.Errorf("read %s/%s: %w", docs.Dir, p, err)
					cancel()
				}
				return
			}
			out[p] = string(data)
		}()
	}
	wg.Wait()
	return out, first
}

// unavailable logs what GitHub said and answers without it: GitHub's text
// can name repos and the App.
func (s *docsService) unavailable(ctx context.Context, svc *forgev1.Service, err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	s.log.WarnContext(ctx, "docs: reading from GitHub failed", "service", svc.GetName(), "error", err)
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("the docs of %s cannot be read from GitHub right now", svc.GetName()))
}

// service returns the catalog entry named by a request and its docs.
func (s *docsService) service(ctx context.Context, name string) (*forgev1.Service, *docSet, error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, nil, err
	}
	if s.gh == nil {
		return nil, nil, errNoDocsGitHub
	}
	if _, err := parseName(name); err != nil {
		return nil, nil, err
	}
	svc, err := s.catalog.get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	set, err := s.load(ctx, svc)
	if err != nil {
		return nil, nil, err
	}
	if set.none != "" {
		return nil, nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no docs: %s", name, set.none))
	}
	return svc, set, nil
}

func docSetMessage(svc *forgev1.Service, set *docSet) *forgev1.DocSet {
	ds := &forgev1.DocSet{
		Service:      svc.GetName(),
		OwnerTeam:    svc.GetOwnerTeam(),
		PageCount:    int32(len(set.pages)), //nolint:gosec // at most docs.MaxPages
		Commit:       set.commit,
		ChangeCommit: set.change.SHA,
		RepoUrl:      svc.GetRepoUrl(),
		Branch:       set.branch,
	}
	if !set.change.Time.IsZero() {
		ds.ChangeTime = timestamppb.New(set.change.Time)
	}
	return ds
}

func (s *docsService) ListDocSets(ctx context.Context, _ *connect.Request[forgev1.ListDocSetsRequest]) (*connect.Response[forgev1.ListDocSetsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	if s.gh == nil {
		return nil, errNoDocsGitHub
	}
	ids, err := s.catalog.ids(ctx)
	if err != nil {
		return nil, err
	}
	sets := make([]*forgev1.DocSet, len(ids))
	var wg sync.WaitGroup
	slots := make(chan struct{}, docsReads)
	for i, id := range ids {
		svc, err := s.catalog.get(ctx, servicePrefix+id)
		if err != nil {
			continue // deleted since the list was read
		}
		// Docs read before are answered at once; a stale set is read again
		// in the background, so the list never waits on GitHub for them.
		if snap, fresh := s.fresh(svc); snap != nil {
			if !fresh {
				s.refreshLater(svc)
			}
			if snap.set.none == "" {
				sets[i] = docSetMessage(svc, snap.set)
			}
			continue
		}
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			set, err := s.load(ctx, svc)
			if err != nil {
				s.log.WarnContext(ctx, "docs: left out of the list", "service", svc.GetName(), "error", err)
				return
			}
			if set.none == "" {
				sets[i] = docSetMessage(svc, set)
			}
		}()
	}
	wg.Wait()
	out := &forgev1.ListDocSetsResponse{}
	for _, ds := range sets {
		if ds != nil {
			out.DocSets = append(out.DocSets, ds)
		}
	}
	return connect.NewResponse(out), nil
}

// refreshLater reads svc's docs again without a request waiting for it.
func (s *docsService) refreshLater(svc *forgev1.Service) {
	e := s.entry(svc.GetName())
	if !e.refreshing.CompareAndSwap(false, true) {
		return
	}
	s.background(func() {
		defer e.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), docsRefreshTimeout)
		defer cancel()
		if _, err := s.load(ctx, svc); err != nil {
			s.log.WarnContext(ctx, "docs: refresh failed", "service", svc.GetName(), "error", err)
		}
	})
}

func (s *docsService) GetDocSet(ctx context.Context, req *connect.Request[forgev1.GetDocSetRequest]) (*connect.Response[forgev1.GetDocSetResponse], error) {
	svc, set, err := s.service(ctx, req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	ds := docSetMessage(svc, set)
	for _, p := range set.pages {
		ds.Pages = append(ds.Pages, &forgev1.DocPageRef{Path: p.Path, Title: p.Title})
	}
	return connect.NewResponse(&forgev1.GetDocSetResponse{DocSet: ds}), nil
}

// docPath checks the path of a request, relative to docs/.
func docPath(p string) (string, error) {
	if _, err := docs.CleanPath(p); err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path: %w", err))
	}
	return p, nil
}

func (s *docsService) GetDocPage(ctx context.Context, req *connect.Request[forgev1.GetDocPageRequest]) (*connect.Response[forgev1.GetDocPageResponse], error) {
	p := req.Msg.GetPath()
	if p == "" {
		p = docs.Start
	}
	if _, err := docPath(p); err != nil {
		return nil, err
	}
	if !docs.IsPage(p) {
		return nil, invalidArg("path %q is not a page: pages end in .md", p)
	}
	svc, set, err := s.service(ctx, req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	md, ok := set.content[p]
	if !ok {
		if en, exists := set.files[p]; exists {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/%s is %d bytes; pages are read up to %d", docs.Dir, p, en.Size, docs.MaxPageBytes))
		}
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no page %s/%s", svc.GetName(), docs.Dir, p))
	}
	page := &forgev1.DocPage{
		Service:  svc.GetName(),
		Path:     p,
		Title:    docs.Title([]byte(md), p),
		Markdown: md,
		Commit:   set.commit,
		EditUrl:  editURL(set, p),
	}
	for _, ref := range set.pages {
		if ref.Path == p {
			page.Title = ref.Title
		}
	}
	if !set.change.Time.IsZero() {
		page.ChangeTime = timestamppb.New(set.change.Time)
	}
	return connect.NewResponse(&forgev1.GetDocPageResponse{Page: page}), nil
}

// editURL is the page's editor on GitHub, on the default branch.
func editURL(set *docSet, p string) string {
	segs := strings.Split(docs.Dir+"/"+p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return fmt.Sprintf("https://github.com/%s/%s/edit/%s/%s", url.PathEscape(set.owner), url.PathEscape(set.repo), strings.ReplaceAll(url.PathEscape(set.branch), "%2F", "/"), strings.Join(segs, "/"))
}

func (s *docsService) GetDocAsset(ctx context.Context, req *connect.Request[forgev1.GetDocAssetRequest]) (*connect.Response[forgev1.GetDocAssetResponse], error) {
	p, err := docPath(req.Msg.GetPath())
	if err != nil {
		return nil, err
	}
	typ, ok := docs.AssetType(p)
	if !ok {
		return nil, invalidArg("path %q is not an image: images are png, jpeg, gif, webp or svg", p)
	}
	svc, set, err := s.service(ctx, req.Msg.GetService())
	if err != nil {
		return nil, err
	}
	en, ok := set.files[p]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no image %s/%s", svc.GetName(), docs.Dir, p))
	}
	if en.Size > docs.MaxAssetBytes {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/%s is %d bytes; images are read up to %d", docs.Dir, p, en.Size, docs.MaxAssetBytes))
	}
	data, err := s.asset(ctx, svc, set, en.SHA)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.GetDocAssetResponse{MediaType: typ, Data: data, Commit: set.commit}), nil
}

// asset reads a blob, kept in memory by its SHA: a blob never changes.
func (s *docsService) asset(ctx context.Context, svc *forgev1.Service, set *docSet, sha string) ([]byte, error) {
	s.mu.Lock()
	data, ok := s.assets[sha]
	s.mu.Unlock()
	if ok {
		return data, nil
	}
	data, err := s.gh.Blob(ctx, set.owner, set.repo, sha)
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.assetBytes+len(data) > docsMaxAssetCache {
		s.assets, s.assetBytes = map[string][]byte{}, 0
	}
	s.assets[sha] = data
	s.assetBytes += len(data)
	return data, nil
}
