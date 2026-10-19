package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/docs"
	"github.com/showmino/forge/internal/envtest"
)

type docsFixture struct {
	s   *docsService
	gh  *envtest.DocsGitHub
	now time.Time
}

func newDocsFixture(t *testing.T) *docsFixture {
	t.Helper()
	cat := NewCatalog(nil)
	for _, reg := range []*forgev1.RegisterServiceRequest{
		{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"}},
		{ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/ledger"}},
		{ServiceId: "elsewhere", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://gitlab.com/acme/elsewhere"}},
	} {
		if _, err := cat.RegisterService(writerCtx(), connect.NewRequest(reg)); err != nil {
			t.Fatal(err)
		}
	}
	f := &docsFixture{gh: &envtest.DocsGitHub{}, now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	f.gh.Push("showmino", "payments", map[string]string{
		"forge.yaml":         "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\ndocs:\n  - path: runbook.md\n    title: When it breaks\n  - path: api/errors.md\n",
		"docs/index.md":      "# Payments\nCharges and refunds. See the [runbook](runbook.md).\n",
		"docs/runbook.md":    "# Runbook\n",
		"docs/api/errors.md": "# Errors\n",
		"docs/notes.md":      "# Notes\n",
		"docs/img/flow.png":  "png!",
		"docs/img/flow.html": "<p>",
		"docs/huge.md":       strings.Repeat("x", docs.MaxPageBytes+1),
		"README.md":          "# payments",
	}, f.now.Add(-time.Hour))
	f.gh.Push("showmino", "ledger", map[string]string{"README.md": "no docs here"}, f.now)
	f.s = newDocsService(f.gh, cat, "showmino", slog.New(slog.DiscardHandler))
	f.s.now = func() time.Time { return f.now }
	f.s.background = func(run func()) { run() }
	return f
}

func readerCtx() context.Context {
	return context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Permissions: []string{PermServicesRead}})
}

func TestDocSetOrderAndPages(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	ctx := readerCtx()
	resp, err := f.s.GetDocSet(ctx, connect.NewRequest(&forgev1.GetDocSetRequest{Service: "services/payments"}))
	if err != nil {
		t.Fatal(err)
	}
	ds := resp.Msg.GetDocSet()
	var got []string
	for _, p := range ds.GetPages() {
		got = append(got, p.GetPath()+"="+p.GetTitle())
	}
	// forge.yaml lists the order and a title; the start page comes first.
	if strings.Join(got, " ") != "index.md=Payments runbook.md=When it breaks api/errors.md=Errors" {
		t.Errorf("pages = %v", got)
	}
	if ds.GetBranch() != "main" || ds.GetCommit() == "" || ds.GetChangeTime().AsTime() != f.now.Add(-time.Hour) || ds.GetOwnerTeam() != "billing" || ds.GetPageCount() != 3 {
		t.Errorf("set = %+v", ds)
	}

	page, err := f.s.GetDocPage(ctx, connect.NewRequest(&forgev1.GetDocPageRequest{Service: "services/payments"}))
	if err != nil {
		t.Fatal(err)
	}
	p := page.Msg.GetPage()
	if p.GetPath() != "index.md" || p.GetTitle() != "Payments" || !strings.Contains(p.GetMarkdown(), "Charges and refunds") ||
		p.GetEditUrl() != "https://github.com/showmino/payments/edit/main/docs/index.md" || p.GetCommit() != ds.GetCommit() {
		t.Errorf("start page = %+v", p)
	}
	// A page not in the list is still read by its path.
	page, err = f.s.GetDocPage(ctx, connect.NewRequest(&forgev1.GetDocPageRequest{Service: "services/payments", Path: "notes.md"}))
	if err != nil || page.Msg.GetPage().GetTitle() != "Notes" {
		t.Errorf("notes.md = %v, %v", page, err)
	}
	asset, err := f.s.GetDocAsset(ctx, connect.NewRequest(&forgev1.GetDocAssetRequest{Service: "services/payments", Path: "img/flow.png"}))
	if err != nil || asset.Msg.GetMediaType() != "image/png" || string(asset.Msg.GetData()) != "png!" {
		t.Errorf("asset = %v, %v", asset, err)
	}
}

func TestDocsRefusals(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	ctx := readerCtx()
	page := func(svc, p string) error {
		_, err := f.s.GetDocPage(ctx, connect.NewRequest(&forgev1.GetDocPageRequest{Service: svc, Path: p}))
		return err
	}
	asset := func(p string) error {
		_, err := f.s.GetDocAsset(ctx, connect.NewRequest(&forgev1.GetDocAssetRequest{Service: "services/payments", Path: p}))
		return err
	}
	for _, c := range []struct {
		what string
		err  error
		code connect.Code
		text string
	}{
		{"outside docs/", page("services/payments", "../forge.yaml"), connect.CodeInvalidArgument, "inside docs/"},
		{"absolute", page("services/payments", "/docs/index.md"), connect.CodeInvalidArgument, "relative"},
		{"not a page", page("services/payments", "img/flow.png"), connect.CodeInvalidArgument, "not a page"},
		{"missing page", page("services/payments", "gone.md"), connect.CodeNotFound, "no page docs/gone.md"},
		{"too big", page("services/payments", "huge.md"), connect.CodeFailedPrecondition, "pages are read up to"},
		{"no docs folder", page("services/ledger", ""), connect.CodeNotFound, "has no docs/ folder"},
		{"repo elsewhere", page("services/elsewhere", ""), connect.CodeNotFound, "not a repo of the showmino organization"},
		{"no service", page("services/nope", ""), connect.CodeNotFound, ""},
		{"not an image", asset("img/flow.html"), connect.CodeInvalidArgument, "not an image"},
		{"svg can carry script", asset("img/logo.svg"), connect.CodeInvalidArgument, "not an image"},
		{"missing image", asset("img/gone.png"), connect.CodeNotFound, "no image"},
	} {
		if connect.CodeOf(c.err) != c.code || !strings.Contains(c.err.Error(), c.text) {
			t.Errorf("%s: %v, want %v with %q", c.what, c.err, c.code, c.text)
		}
	}

	if _, err := f.s.GetDocSet(context.Background(), connect.NewRequest(&forgev1.GetDocSetRequest{Service: "services/payments"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("without a login: %v", err)
	}
	none := newDocsService(nil, NewCatalog(nil), "showmino", slog.New(slog.DiscardHandler))
	if _, err := none.ListDocSets(ctx, connect.NewRequest(&forgev1.ListDocSetsRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("without GitHub: %v", err)
	}
}

func TestDocsCache(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	ctx := readerCtx()
	title := func() string {
		resp, err := f.s.GetDocPage(ctx, connect.NewRequest(&forgev1.GetDocPageRequest{Service: "services/payments"}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetPage().GetTitle()
	}
	if title() != "Payments" {
		t.Fatal("first read")
	}
	reads := f.gh.Calls["Blob"]

	// Within a minute GitHub is not asked again, even when the head moved.
	f.gh.Push("showmino", "payments", map[string]string{"docs/index.md": "# Payments v2\n"}, f.now)
	f.now = f.now.Add(30 * time.Second)
	if title() != "Payments" || f.gh.Calls["HeadSHA"] != 1 {
		t.Errorf("within a minute: %d head checks", f.gh.Calls["HeadSHA"])
	}
	// After a minute the moved head is read again.
	f.now = f.now.Add(time.Minute)
	if title() != "Payments v2" {
		t.Error("the moved head was not read")
	}
	reads2 := f.gh.Calls["Blob"]
	if reads2 == reads {
		t.Error("no blob was read for the new head")
	}
	// An unchanged head is asked about, and nothing else is read.
	f.now = f.now.Add(2 * time.Minute)
	if title() != "Payments v2" || f.gh.Calls["Blob"] != reads2 || f.gh.Calls["Tree"] != 4 {
		t.Errorf("unchanged head: blobs %d→%d, trees %d", reads2, f.gh.Calls["Blob"], f.gh.Calls["Tree"])
	}
	// GitHub down: the docs read before are still served.
	f.gh.Fail = errors.New("connection refused")
	f.now = f.now.Add(2 * time.Minute)
	if title() != "Payments v2" {
		t.Error("not served while GitHub is down")
	}
}

func TestDocsReuseAndRetry(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	ctx := readerCtx()
	get := func(svc string) error {
		_, err := f.s.GetDocSet(ctx, connect.NewRequest(&forgev1.GetDocSetRequest{Service: svc}))
		return err
	}
	if err := get("services/payments"); err != nil {
		t.Fatal(err)
	}
	blobs := f.gh.Calls["Blob"]

	// A push that does not touch docs/ or forge.yaml reads one tree and no file.
	files := map[string]string{
		"forge.yaml":         "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\ndocs:\n  - path: runbook.md\n    title: When it breaks\n  - path: api/errors.md\n",
		"docs/index.md":      "# Payments\nCharges and refunds. See the [runbook](runbook.md).\n",
		"docs/runbook.md":    "# Runbook\n",
		"docs/api/errors.md": "# Errors\n",
		"docs/notes.md":      "# Notes\n",
		"docs/img/flow.png":  "png!",
		"docs/img/flow.html": "<p>",
		"docs/huge.md":       strings.Repeat("x", docs.MaxPageBytes+1),
		"README.md":          "# payments, now with a badge",
	}
	f.gh.Push("showmino", "payments", files, f.now)
	trees := f.gh.Calls["Tree"]
	f.now = f.now.Add(2 * time.Minute)
	if err := get("services/payments"); err != nil || f.gh.Calls["Blob"] != blobs || f.gh.Calls["Tree"] != trees+1 {
		t.Errorf("unchanged docs: %v, blobs %d→%d, trees %d→%d", err, blobs, f.gh.Calls["Blob"], trees, f.gh.Calls["Tree"])
	}
	// One changed page is the only file read again.
	files["docs/runbook.md"] = "# Runbook v2\n"
	f.gh.Push("showmino", "payments", files, f.now)
	f.now = f.now.Add(2 * time.Minute)
	if err := get("services/payments"); err != nil || f.gh.Calls["Blob"] != blobs+2 { // the page and forge.yaml
		t.Errorf("one page changed: %v, blobs %d→%d", err, blobs, f.gh.Calls["Blob"])
	}

	// A read that failed is answered from the cache, and tried again soon.
	g := newDocsFixture(t)
	g.gh.Fail = errors.New("down")
	for range 3 {
		if err := getFrom(g, "services/payments"); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Errorf("GitHub down: %v", err)
		}
	}
	if g.gh.Calls["HeadSHA"] != 1 {
		t.Errorf("GitHub was asked %d times", g.gh.Calls["HeadSHA"])
	}
	g.gh.Fail = nil
	g.now = g.now.Add(docsRetry)
	if err := getFrom(g, "services/payments"); err != nil {
		t.Errorf("after the retry: %v", err)
	}
}

func TestDocsBackgroundRefresh(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	f.s.now = time.Now
	f.s.background = func(run func()) { go run() }
	ctx := readerCtx()
	count := func() int {
		resp, err := f.s.ListDocSets(ctx, connect.NewRequest(&forgev1.ListDocSetsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return len(resp.Msg.GetDocSets())
	}
	if count() != 1 {
		t.Fatal("first list")
	}
	f.gh.Push("showmino", "ledger", map[string]string{"docs/index.md": "# Ledger\n"}, time.Now())
	// Make every entry stale, then list until the refresh behind it lands.
	for _, name := range []string{"services/payments", "services/ledger"} {
		e := f.s.entry(name)
		snap := *e.snap.Load()
		snap.checked = time.Now().Add(-2 * docsFresh)
		e.snap.Store(&snap)
	}
	deadline := time.Now().Add(5 * time.Second)
	for count() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getFrom(f *docsFixture, svc string) error {
	_, err := f.s.GetDocSet(readerCtx(), connect.NewRequest(&forgev1.GetDocSetRequest{Service: svc}))
	return err
}

func TestListDocSets(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	ctx := readerCtx()
	list := func() []string {
		resp, err := f.s.ListDocSets(ctx, connect.NewRequest(&forgev1.ListDocSetsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ds := range resp.Msg.GetDocSets() {
			out = append(out, ds.GetService())
			if len(ds.GetPages()) != 0 {
				t.Error("the list carries pages")
			}
		}
		return out
	}
	if got := list(); strings.Join(got, " ") != "services/payments" {
		t.Errorf("list = %v", got)
	}
	// ledger gets docs; its stale entry is refreshed (inline in this test).
	f.gh.Push("showmino", "ledger", map[string]string{"docs/index.md": "# Ledger\n"}, f.now)
	if got := list(); len(got) != 1 {
		t.Errorf("within a minute: %v", got)
	}
	f.now = f.now.Add(2 * time.Minute)
	list() // answers from the cache and refreshes behind it
	if got := list(); strings.Join(got, " ") != "services/ledger services/payments" {
		t.Errorf("after the refresh: %v", got)
	}
	// GitHub down for a service never read: it is left out, not an error.
	g := newDocsFixture(t)
	g.gh.Fail = errors.New("down")
	resp, err := g.s.ListDocSets(ctx, connect.NewRequest(&forgev1.ListDocSetsRequest{}))
	if err != nil || len(resp.Msg.GetDocSets()) != 0 {
		t.Errorf("GitHub down: %v, %v", resp, err)
	}
}

func TestDocsUnavailableHidesGitHub(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	f.gh.Fail = errors.New("GET /repos/showmino/payments: 500 secret detail")
	_, err := f.s.GetDocSet(readerCtx(), connect.NewRequest(&forgev1.GetDocSetRequest{Service: "services/payments"}))
	if connect.CodeOf(err) != connect.CodeUnavailable || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchDocs(t *testing.T) {
	t.Parallel()
	f := newDocsFixture(t)
	ctx := readerCtx()
	search := func(req *forgev1.SearchDocsRequest) (*forgev1.SearchDocsResponse, error) {
		resp, err := f.s.SearchDocs(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	pages := func(r *forgev1.SearchDocsResponse) string {
		var out []string
		for _, res := range r.GetResults() {
			out = append(out, strings.TrimPrefix(res.GetService(), "services/")+"/"+res.GetPath())
		}
		return strings.Join(out, " ")
	}

	r, err := search(&forgev1.SearchDocsRequest{Query: "REFUNDS charges"})
	if err != nil {
		t.Fatal(err)
	}
	if pages(r) != "payments/index.md" || r.GetTotalSize() != 1 {
		t.Fatalf("refunds = %v", r)
	}
	res := r.GetResults()[0]
	if res.GetTitle() != "Payments" || len(res.GetLines()) != 1 || res.GetLines()[0].GetNumber() != 2 ||
		res.GetLines()[0].GetText() != "Charges and refunds. See the [runbook](runbook.md)." {
		t.Errorf("result = %+v", res)
	}

	// The page named after the word comes first; its title is forge.yaml's.
	r, err = search(&forgev1.SearchDocsRequest{Query: "runbook"})
	if err != nil || pages(r) != "payments/runbook.md payments/index.md" || r.GetResults()[0].GetTitle() != "When it breaks" {
		t.Errorf("runbook = %v, %v", r, err)
	}
	// page_size cuts the list, total_size still counts every page.
	r, err = search(&forgev1.SearchDocsRequest{Query: "runbook", PageSize: 1})
	if err != nil || pages(r) != "payments/runbook.md" || r.GetTotalSize() != 2 {
		t.Errorf("page_size 1 = %v, %v", r, err)
	}
	// A page the docs: list leaves out is found too; one over the size limit is not.
	r, err = search(&forgev1.SearchDocsRequest{Query: "notes"})
	if err != nil || pages(r) != "payments/notes.md" {
		t.Errorf("notes = %v, %v", r, err)
	}
	r, err = search(&forgev1.SearchDocsRequest{Query: "xxxx"})
	if err != nil || len(r.GetResults()) != 0 || r.GetTotalSize() != 0 {
		t.Errorf("huge page = %v, %v", r, err)
	}

	// A page pushed a minute ago is found: search checks stale docs first.
	f.gh.Push("showmino", "ledger", map[string]string{"docs/index.md": "# Ledger\n\nDouble-entry bookkeeping.\n"}, f.now)
	f.now = f.now.Add(docsFresh + time.Second)
	r, err = search(&forgev1.SearchDocsRequest{Query: "bookkeeping"})
	if err != nil || pages(r) != "ledger/index.md" {
		t.Errorf("new page = %v, %v", r, err)
	}
	// One service's docs only.
	r, err = search(&forgev1.SearchDocsRequest{Query: "index", Service: "services/ledger"})
	if err != nil || pages(r) != "" {
		t.Errorf("ledger only = %v, %v", r, err)
	}
	r, err = search(&forgev1.SearchDocsRequest{Query: "bookkeeping", Service: "services/ledger"})
	if err != nil || pages(r) != "ledger/index.md" {
		t.Errorf("ledger only = %v, %v", r, err)
	}

	for _, c := range []struct {
		req  *forgev1.SearchDocsRequest
		code connect.Code
	}{
		{&forgev1.SearchDocsRequest{Query: "   "}, connect.CodeInvalidArgument},
		{&forgev1.SearchDocsRequest{Query: strings.Repeat("a", 201)}, connect.CodeInvalidArgument},
		{&forgev1.SearchDocsRequest{Query: "a", PageSize: -1}, connect.CodeInvalidArgument},
		{&forgev1.SearchDocsRequest{Query: "a", Service: "services/elsewhere"}, connect.CodeNotFound},
		{&forgev1.SearchDocsRequest{Query: "a", Service: "services/Bad Name"}, connect.CodeInvalidArgument},
	} {
		if _, err := search(c.req); connect.CodeOf(err) != c.code {
			t.Errorf("%+v: %v, want %v", c.req, err, c.code)
		}
	}
	if _, err := f.s.SearchDocs(context.Background(), connect.NewRequest(&forgev1.SearchDocsRequest{Query: "a"})); err == nil {
		t.Error("searched without a login")
	}
}
