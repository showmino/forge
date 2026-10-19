package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
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
	HeadSHA(ctx context.Context, owner, repo string) (string, error)
	DefaultBranch(ctx context.Context, owner, name string) (string, error)
	Tree(ctx context.Context, owner, repo, treeish string, recursive bool) (*github.Tree, error)
	Blob(ctx context.Context, owner, repo, sha string) ([]byte, error)
	LastChange(ctx context.Context, owner, repo, ref, path string) (github.CommitInfo, error)
}

const (
	// docsFresh is how long a service's docs are served before GitHub is
	// asked whether its default branch moved. A failed read is kept as long.
	docsFresh = time.Minute
	// docsRetry is how soon a read that GitHub could not answer is tried again.
	docsRetry = 10 * time.Second
	// docsMaxCacheBytes caps the pages kept in memory, across services; the
	// docs read least recently go first.
	docsMaxCacheBytes = 256 << 20
	// docsMaxAssetCache caps the images kept in memory, across services.
	docsMaxAssetCache = 64 << 20
	// docsMaxManifestBytes is the largest forge.yaml read for its docs: list.
	docsMaxManifestBytes = 64 << 10
	// docsReads is how many files are read from GitHub at once, across services.
	docsReads = 8
	// docsListLoads is how many services ListDocSets reads at once when it has
	// never read them.
	docsListLoads = 4
	// docsRefreshTimeout bounds a refresh that no request waits for.
	docsRefreshTimeout = 30 * time.Second
	// docsSearches is how many searches run at once: each reads every page
	// kept in memory.
	docsSearches = 4
	// docsStaleWait is how long a search waits for GitHub about docs it has
	// read before; past it the search answers with those.
	docsStaleWait = 10 * time.Second
)

// docSet is one service's docs as read at one commit. none is set when the
// repo has no docs, with the reason.
type docSet struct {
	owner, repo        string
	branch, commit     string
	docsTree, manifest string
	change             github.CommitInfo
	pages              []docs.Page
	content            map[string]string
	// heads are the pages' own titles: the first heading, else the file name.
	heads map[string]string
	files map[string]github.TreeEntry
	bytes int64
	none  string
}

// docsEntry is the cache of one service. mu lets one refresh run at a time;
// snap is read without it, so a request never waits for another's refresh.
type docsEntry struct {
	mu         sync.Mutex
	snap       atomic.Pointer[docsSnap]
	refreshing atomic.Bool
	// used is when a request last asked for these docs (Unix nanoseconds).
	used atomic.Int64
}

// docsSnap is what a read gave: the docs, or the error to answer with until
// the next check.
type docsSnap struct {
	repoURL string
	set     *docSet
	err     error
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
	// slots bounds the reads from GitHub that run at once.
	slots chan struct{}
	// searches bounds the searches that run at once.
	searches chan struct{}

	mu         sync.Mutex
	entries    map[string]*docsEntry
	assets     map[string][]byte
	assetBytes int
}

func newDocsService(gh DocsGitHub, catalog *Catalog, org string, log *slog.Logger) *docsService {
	return &docsService{
		gh: gh, catalog: catalog, org: org, log: log, now: time.Now,
		background: func(f func()) { go f() },
		slots:      make(chan struct{}, docsReads),
		searches:   make(chan struct{}, docsSearches),
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
	if snap == nil || snap.repoURL != svc.GetRepoUrl() {
		return nil, false
	}
	return snap, s.now().Before(snap.checked.Add(docsFresh))
}

// load returns svc's docs, asking GitHub whether the default branch moved
// at most once a minute, and reading the files again only when docs/ or
// forge.yaml changed. When GitHub cannot be asked, the docs read before are
// served; a read that failed is answered from the cache too, until the next
// check.
func (s *docsService) load(ctx context.Context, svc *forgev1.Service) (*docSet, error) {
	e := s.entry(svc.GetName())
	e.used.Store(s.now().UnixNano())
	if snap, ok := s.fresh(svc); ok {
		return snap.set, snap.err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if snap, ok := s.fresh(svc); ok {
		return snap.set, snap.err
	}
	var prev *docSet
	if old := e.snap.Load(); old != nil && old.repoURL == svc.GetRepoUrl() {
		prev = old.set
	}
	set, err := s.read(ctx, svc, prev)
	snap := &docsSnap{repoURL: svc.GetRepoUrl(), set: set, err: err, checked: s.now()}
	switch {
	case err != nil && prev != nil:
		s.log.WarnContext(ctx, "docs: serving the docs read before", "service", svc.GetName(), "error", err)
		snap.set, snap.err = prev, nil
	case connect.CodeOf(err) == connect.CodeUnavailable:
		// GitHub may answer again soon: do not wait a whole minute to ask.
		snap.checked = snap.checked.Add(docsRetry - docsFresh)
	}
	e.snap.Store(snap)
	s.trim(svc.GetName())
	return snap.set, snap.err
}

// trim drops the docs read least recently until the pages of all services
// fit in docsMaxCacheBytes again. keep, the service just read, stays.
func (s *docsService) trim(keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, e := range s.entries {
		if snap := e.snap.Load(); snap != nil && snap.set != nil {
			total += snap.set.bytes
		}
	}
	for total > docsMaxCacheBytes {
		oldest, at := "", int64(0)
		for name, e := range s.entries {
			snap := e.snap.Load()
			if name == keep || snap == nil || snap.set == nil || snap.set.bytes == 0 {
				continue
			}
			if u := e.used.Load(); oldest == "" || u < at {
				oldest, at = name, u
			}
		}
		if oldest == "" {
			return
		}
		total -= s.entries[oldest].snap.Load().set.bytes
		delete(s.entries, oldest)
	}
}

// forget drops the docs of services that are no longer in the catalog.
func (s *docsService) forget(ids []string) {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[servicePrefix+id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.entries {
		if !keep[name] {
			delete(s.entries, name)
		}
	}
}

// read reads svc's docs at the head of its repo's default branch. prev is
// what was read before: kept as it is when the head has not moved, and with
// the new head when docs/ and forge.yaml are the same.
func (s *docsService) read(ctx context.Context, svc *forgev1.Service, prev *docSet) (*docSet, error) {
	none := func(why string) *docSet { return &docSet{none: why} }
	owner, repo, ok := repoOf(svc.GetRepoUrl(), s.org)
	if !ok {
		return none("its repo is not a repo of the " + s.org + " organization on GitHub"), nil
	}
	head, err := s.gh.HeadSHA(ctx, owner, repo)
	if errors.Is(err, github.ErrNotFound) {
		return none("its repo cannot be read, or has no commits"), nil
	}
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	if prev != nil && prev.commit == head {
		return prev, nil
	}
	root, err := s.gh.Tree(ctx, owner, repo, head, false)
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	var docsTree string
	var forgeYAML github.TreeEntry
	for _, en := range root.Entries {
		switch {
		case en.Path == docs.Dir && en.Type == "tree":
			docsTree = en.SHA
		case en.Path == manifest.FileName && en.Type == "blob":
			forgeYAML = en
		}
	}
	if docsTree == "" {
		return &docSet{owner: owner, repo: repo, commit: head, none: "its repo has no " + docs.Dir + "/ folder"}, nil
	}
	if prev != nil && prev.none == "" && prev.docsTree == docsTree && prev.manifest == forgeYAML.SHA {
		// Another part of the repo changed: the docs are the same.
		same := *prev
		same.commit = head
		return &same, nil
	}

	set := &docSet{owner: owner, repo: repo, commit: head, docsTree: docsTree, manifest: forgeYAML.SHA}
	tree, err := s.gh.Tree(ctx, owner, repo, docsTree, true)
	if err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	if tree.Truncated {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/ in %s/%s has more files than GitHub lists at once", docs.Dir, owner, repo))
	}
	set.files = map[string]github.TreeEntry{}
	var pages []string
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
			set.bytes += en.Size
		}
	}
	if _, ok := set.files[docs.Start]; !ok {
		set.none = "its repo has no " + docs.Dir + "/" + docs.Start
		return set, nil
	}
	if len(pages) > docs.MaxPages {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s/ in %s/%s has %d pages; at most %d are read", docs.Dir, owner, repo, len(pages), docs.MaxPages))
	}
	if set.bytes > docs.MaxSetBytes {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the pages in %s/ of %s/%s are %d bytes together; at most %d are read", docs.Dir, owner, repo, set.bytes, docs.MaxSetBytes))
	}
	if set.content, err = s.readPages(ctx, set, pages, prev); err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}

	var list []docs.Entry
	switch {
	case forgeYAML.SHA == "":
	case forgeYAML.Size > docsMaxManifestBytes:
		s.log.WarnContext(ctx, "docs: forge.yaml is too big to read, using the default page order", "service", svc.GetName(), "bytes", forgeYAML.Size)
	default:
		data, err := s.gh.Blob(ctx, owner, repo, forgeYAML.SHA)
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
	set.heads = make(map[string]string, len(set.content))
	for p, md := range set.content {
		set.heads[p] = docs.Title([]byte(md), p)
	}
	set.pages = docs.Order(pages, list, func(p string) string { return set.heads[p] })

	if set.branch, err = s.gh.DefaultBranch(ctx, owner, repo); err != nil {
		return nil, s.unavailable(ctx, svc, err)
	}
	if set.change, err = s.gh.LastChange(ctx, owner, repo, head, docs.Dir); err != nil && !errors.Is(err, github.ErrNotFound) {
		return nil, s.unavailable(ctx, svc, err)
	}
	return set, nil
}

// readPages reads the pages' blobs, a few at a time across all services. A
// page whose blob prev read already is not read again.
func (s *docsService) readPages(ctx context.Context, set *docSet, pages []string, prev *docSet) (map[string]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		out   = make(map[string]string, len(pages))
		first error
		wg    sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = err
		}
		cancel()
	}
	for _, p := range pages {
		sha := set.files[p].SHA
		if prev != nil && prev.files[p].SHA == sha {
			if md, ok := prev.content[p]; ok {
				out[p] = md
				continue
			}
		}
		select {
		case s.slots <- struct{}{}:
		case <-ctx.Done():
			fail(ctx.Err())
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer func() { <-s.slots; wg.Done() }()
			data, err := s.gh.Blob(ctx, set.owner, set.repo, sha)
			if err != nil {
				fail(fmt.Errorf("read %s/%s: %w", docs.Dir, p, err))
				return
			}
			mu.Lock()
			defer mu.Unlock()
			// A page in another encoding would make every answer holding a
			// line of it fail to marshal.
			out[p] = strings.ToValidUTF8(string(data), "\uFFFD")
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
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
	all, err := s.all(ctx, false)
	if err != nil {
		return nil, err
	}
	out := &forgev1.ListDocSetsResponse{}
	for _, d := range all {
		out.DocSets = append(out.DocSets, docSetMessage(d.svc, d.set))
	}
	return connect.NewResponse(out), nil
}

// serviceDocs is one service and its docs.
type serviceDocs struct {
	svc *forgev1.Service
	set *docSet
}

// all returns the docs of every service in the catalog that has docs, in
// the catalog's order. Docs never read are read, a few services at once, and
// a service whose read fails is left out. Stale docs are read again: before
// answering when wait is set, else in the background, answering with the
// docs read before so the caller never waits on GitHub for them.
func (s *docsService) all(ctx context.Context, wait bool) ([]serviceDocs, error) {
	ids, err := s.catalog.ids(ctx)
	if err != nil {
		return nil, err
	}
	s.forget(ids)
	found := make([]serviceDocs, len(ids))
	var wg sync.WaitGroup
	slots := make(chan struct{}, docsListLoads)
	for i, id := range ids {
		svc, err := s.catalog.get(ctx, servicePrefix+id)
		if err != nil {
			continue // deleted since the list was read
		}
		snap, fresh := s.fresh(svc)
		if snap != nil && (fresh || !wait) {
			if !fresh {
				s.refreshLater(svc)
			}
			if snap.set != nil && snap.set.none == "" {
				found[i] = serviceDocs{svc, snap.set}
			}
			continue
		}
		// The read has a limit of its own and outlives a caller that goes:
		// a cancelled request must not be remembered as GitHub failing.
		limit := docsRefreshTimeout
		if snap != nil && snap.set != nil {
			limit = docsStaleWait // load answers with the docs read before
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
			defer cancel()
			set, err := s.load(lctx, svc)
			if err != nil {
				s.log.WarnContext(ctx, "docs: left out", "service", svc.GetName(), "error", err)
				return
			}
			if set.none == "" {
				found[i] = serviceDocs{svc, set}
			}
		}()
	}
	wg.Wait()
	out := found[:0]
	for _, d := range found {
		if d.set != nil {
			out = append(out, d)
		}
	}
	return out, nil
}

const (
	// docsSearchMax is the longest search query.
	docsSearchMax = 200
	// docsSearchPage and docsSearchPageMax are the default and the most
	// pages a search returns.
	docsSearchPage    = 20
	docsSearchPageMax = 100
	// docsSearchLines is how many matched lines a result carries.
	docsSearchLines = 3
)

// SearchDocs looks through the pages kept in memory; nothing is indexed.
// Unlike ListDocSets it waits for stale docs to be checked with GitHub, so
// a page merged a minute ago is found.
func (s *docsService) SearchDocs(ctx context.Context, req *connect.Request[forgev1.SearchDocsRequest]) (*connect.Response[forgev1.SearchDocsResponse], error) {
	q := strings.TrimSpace(req.Msg.GetQuery())
	if len(q) > docsSearchMax {
		return nil, invalidArg("query is %d characters; at most %d are searched", len(q), docsSearchMax)
	}
	terms := docs.Terms(q)
	if len(terms) == 0 {
		return nil, invalidArg("query is empty: name the words to look for")
	}
	size := int(req.Msg.GetPageSize())
	switch {
	case size < 0:
		return nil, invalidArg("page_size must not be negative")
	case size == 0:
		size = docsSearchPage
	case size > docsSearchPageMax:
		size = docsSearchPageMax
	}

	var all []serviceDocs
	if name := req.Msg.GetService(); name != "" {
		svc, set, err := s.service(ctx, name)
		if err != nil {
			return nil, err
		}
		all = []serviceDocs{{svc, set}}
	} else {
		if err := require(ctx, PermServicesRead); err != nil {
			return nil, err
		}
		if s.gh == nil {
			return nil, errNoDocsGitHub
		}
		var err error
		if all, err = s.all(ctx, true); err != nil {
			return nil, err
		}
	}

	select {
	case s.searches <- struct{}{}:
		defer func() { <-s.searches }()
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	}
	type hit struct {
		res   *forgev1.DocSearchResult
		match docs.Match
	}
	var hits []hit
	for _, d := range all {
		titles := make(map[string]string, len(d.set.pages))
		for _, p := range d.set.pages {
			titles[p.Path] = p.Title
		}
		// Every page read, also those a docs: list leaves out of the menu,
		// listed pages first in the menu's order, then the rest by path.
		paths := make([]string, 0, len(d.set.content))
		for _, p := range d.set.pages {
			paths = append(paths, p.Path)
		}
		var rest []string
		for p := range d.set.content {
			if _, listed := titles[p]; !listed {
				rest = append(rest, p)
			}
		}
		slices.Sort(rest)
		for _, p := range append(paths, rest...) {
			md := d.set.content[p]
			heading := d.set.heads[p]
			title, ok := titles[p]
			if !ok {
				title = heading
			}
			// The page's own heading counts as its title too when the
			// docs: list names it otherwise.
			m, ok := docs.Find(md, title+"\n"+heading, terms, docsSearchLines)
			if !ok {
				continue
			}
			res := &forgev1.DocSearchResult{Service: d.svc.GetName(), Path: p, Title: title}
			for _, l := range m.Lines {
				res.Lines = append(res.Lines, &forgev1.DocSearchLine{Number: int32(l.Number), Text: l.Text}) //nolint:gosec // a page is at most 1 MiB
			}
			hits = append(hits, hit{res, m})
		}
	}
	// Stable, so equal scores keep the catalog's and the menu's order.
	slices.SortStableFunc(hits, func(a, b hit) int { return docs.Better(a.match, b.match) })
	out := &forgev1.SearchDocsResponse{TotalSize: int32(len(hits))} //nolint:gosec // at most docs.MaxPages per service
	for _, h := range hits[:min(len(hits), size)] {
		out.Results = append(out.Results, h.res)
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
		Title:    set.heads[p],
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
	if _, ok := s.assets[sha]; ok {
		return data, nil
	}
	if s.assetBytes+len(data) > docsMaxAssetCache {
		s.assets, s.assetBytes = map[string][]byte{}, 0
	}
	s.assets[sha] = data
	s.assetBytes += len(data)
	return data, nil
}
