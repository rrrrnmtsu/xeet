package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melqtx/xeet/pkg/api"
	"github.com/melqtx/xeet/pkg/config"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeStore is a saved-account roster with per-account keyring outcomes. It
// never writes, so a test that sees a write would fail to compile against
// the Store interface rather than pass by accident.
type fakeStore struct {
	version  int
	accounts []config.AccountInfo
	secrets  map[string]*config.Config
	loadErr  map[string]error
}

func (f *fakeStore) Version() (int, error) { return f.version, nil }

func (f *fakeStore) Accounts() ([]config.AccountInfo, error) {
	return append([]config.AccountInfo(nil), f.accounts...), nil
}

func (f *fakeStore) LoadAccount(userID string) (*config.Config, error) {
	if err, ok := f.loadErr[userID]; ok {
		return nil, err
	}
	cfg, ok := f.secrets[userID]
	if !ok {
		return nil, fmt.Errorf("saved account %q does not exist", userID)
	}
	copied := *cfg
	return &copied, nil
}

const (
	aliceID     = "1000"
	aliceHandle = "alice"
	bobID       = "2000"
	bobHandle   = "bob"
	aliceToken  = "SECRET-AUTH-TOKEN-ALICE"
	aliceCT0    = "SECRET-CT0-ALICE"
)

func twoAccountStore() *fakeStore {
	imported := time.Date(2026, 8, 6, 2, 9, 36, 0, time.UTC)
	expires := time.Date(2027, 5, 29, 8, 24, 48, 0, time.UTC)
	return &fakeStore{
		version: 2,
		accounts: []config.AccountInfo{
			{UserID: aliceID, Handle: aliceHandle, SessionBrowser: "Chrome", SessionProfile: "Default", SessionImported: imported, Active: true},
			{UserID: bobID, Handle: bobHandle, SessionBrowser: "Firefox"},
		},
		secrets: map[string]*config.Config{
			aliceID: {AuthToken: aliceToken, CT0: aliceCT0, UserID: aliceID, Handle: aliceHandle,
				SessionBrowser: "Chrome", SessionProfile: "Default", SessionImported: imported, SessionExpires: expires},
			bobID: {AuthToken: "SECRET-BOB", CT0: "SECRET-BOB-CT0", UserID: bobID, Handle: bobHandle},
		},
		loadErr: map[string]error{},
	}
}

// page is one scripted upstream response: the posts X returns for a given
// incoming cursor, the cursor it hands back, or the error it fails with.
type page struct {
	posts  []api.TimelinePost
	cursor string
	err    error
}

type fakeSession struct {
	search    map[string]page // keyed by incoming cursor
	bookmarks map[string]page
	viewer    *api.Account
	viewerErr error
	block     bool // hold every call until the context ends

	mu    sync.Mutex
	calls []string
}

func (f *fakeSession) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeSession) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSession) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeSession) serve(ctx context.Context, kind string, pages map[string]page, cursor string) (*api.TimelinePage, error) {
	f.record(kind + ":" + cursor)
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p, ok := pages[cursor]
	if !ok {
		return nil, fmt.Errorf("fixture has no page for cursor %q", cursor)
	}
	if p.err != nil {
		return nil, p.err
	}
	return &api.TimelinePage{Posts: p.posts, Cursor: p.cursor}, nil
}

func (f *fakeSession) FetchSearchTimeline(ctx context.Context, query, cursor string, count int) (*api.TimelinePage, error) {
	return f.serve(ctx, "search", f.search, cursor)
}

func (f *fakeSession) FetchBookmarks(ctx context.Context, cursor string, count int) (*api.TimelinePage, error) {
	return f.serve(ctx, "bookmarks", f.bookmarks, cursor)
}

func (f *fakeSession) FetchViewer(ctx context.Context) (*api.Account, error) {
	f.record("viewer")
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.viewerErr != nil {
		return nil, f.viewerErr
	}
	return f.viewer, nil
}

func post(id, handle, text string, known bool) api.TimelinePost {
	return api.TimelinePost{
		ID: id, Text: text, AuthorName: strings.ToUpper(handle[:1]) + handle[1:], Handle: handle,
		CreatedAt:       time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(len(id)) * time.Hour),
		ReplyCount:      1,
		RepostCount:     2,
		LikeCount:       3,
		EngagementKnown: known,
		ViewCount:       "4",
	}
}

type harness struct {
	t       *testing.T
	session *sdk.ClientSession
	fake    *fakeSession
	now     time.Time
}

func newHarness(t *testing.T, store *fakeStore, fake *fakeSession, mutate func(*Options)) *harness {
	t.Helper()
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	opts := Options{
		Store:           store,
		NewSession:      func(*config.Config) Session { return fake },
		AllowedAccounts: []string{"@" + aliceHandle},
		PageDelay:       0,
		Now:             func() time.Time { return now },
		Version:         "test",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&opts)
	}
	server, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	serverSession, err := server.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return &harness{t: t, session: clientSession, fake: fake, now: now}
}

func (h *harness) call(tool string, args map[string]any) (*sdk.CallToolResult, string) {
	h.t.Helper()
	result, err := h.session.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		h.t.Fatalf("tools/call %s: %v", tool, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		h.t.Fatal(err)
	}
	return result, string(raw)
}

func (h *harness) posts(tool string, args map[string]any) (*sdk.CallToolResult, PostsResult, string) {
	h.t.Helper()
	result, raw := h.call(tool, args)
	var parsed PostsResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		h.t.Fatalf("decode %s result: %v\n%s", tool, err, raw)
	}
	return result, parsed, raw
}

func (h *harness) health(args map[string]any) (*sdk.CallToolResult, HealthResult, string) {
	h.t.Helper()
	result, raw := h.call(ToolSessionHealth, args)
	var parsed HealthResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		h.t.Fatalf("decode health result: %v\n%s", err, raw)
	}
	return result, parsed, raw
}

func assertFailure(t *testing.T, result *sdk.CallToolResult, failure *Failure, status string, code Code) {
	t.Helper()
	if status != StatusError {
		t.Fatalf("status = %q, want %q", status, StatusError)
	}
	if !result.IsError {
		t.Fatalf("isError = false, want true for %s", code)
	}
	if failure == nil || failure.Code != code {
		t.Fatalf("error = %+v, want code %s", failure, code)
	}
}

func TestToolListIsExactlyTheThreeReadTools(t *testing.T) {
	h := newHarness(t, twoAccountStore(), &fakeSession{}, nil)
	listed, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", tool.Name)
		}
		if tool.Annotations != nil && tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
			t.Errorf("%s is annotated destructive", tool.Name)
		}
	}
	want := ToolNames()
	sort.Strings(names)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want exactly %v", names, want)
	}
	if listed.NextCursor != "" {
		t.Fatalf("unexpected second page of tools")
	}
}

func TestUnknownAndMutationToolNamesAreRejectedAtTheProtocol(t *testing.T) {
	h := newHarness(t, twoAccountStore(), &fakeSession{}, nil)
	for _, name := range []string{"post_x", "like_x_post", "create_bookmark", "FetchHomeTimeline", ""} {
		_, err := h.session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err == nil {
			t.Fatalf("tools/call %q succeeded; it must not exist", name)
		}
	}
}

func TestSearchReturnsPostsWithNullableMetricsAndABoundCursor(t *testing.T) {
	fake := &fakeSession{search: map[string]page{
		"": {posts: []api.TimelinePost{post("11", "carol", "first", true), post("222", "dave", "second", false)}, cursor: "X-CURSOR-1"},
	}}
	h := newHarness(t, twoAccountStore(), fake, nil)

	result, parsed, raw := h.posts(ToolSearchPosts, map[string]any{"query": "go tui", "limit": 2})
	if result.IsError {
		t.Fatalf("isError set: %s", raw)
	}
	if parsed.Status != StatusOK || !parsed.Complete || parsed.Count != 2 || parsed.PagesFetched != 1 {
		t.Fatalf("result = %s", raw)
	}
	if parsed.RequestedLimit != 2 || fake.callCount() != 1 {
		t.Fatalf("a page that meets limit must not trigger a second fetch: %s (%v)", raw, fake.calls)
	}
	if parsed.AccountID != aliceID || parsed.AccountHandle != aliceHandle || parsed.Query != "go tui" {
		t.Fatalf("account/query = %s", raw)
	}
	if parsed.FetchedAt != "2026-09-16T09:00:00Z" || parsed.Freshness != "live" || !strings.Contains(parsed.Source, "SearchTimeline") {
		t.Fatalf("provenance = %s", raw)
	}
	first := parsed.Posts[0]
	if first.URL != "https://x.com/carol/status/11" || first.Author.URL != "https://x.com/carol" || first.Author.Name != "Carol" {
		t.Fatalf("first post = %+v", first)
	}
	if first.Metrics.Likes == nil || *first.Metrics.Likes != 3 || first.Metrics.Views == nil || *first.Metrics.Views != 4 {
		t.Fatalf("known metrics = %+v", first.Metrics)
	}
	second := parsed.Posts[1]
	if second.Metrics.Likes != nil || second.Metrics.Replies != nil || second.Metrics.Reposts != nil {
		t.Fatalf("unknown metrics must be null, got %+v", second.Metrics)
	}
	if !strings.Contains(raw, `"likes":null`) {
		t.Fatalf("null metric is not serialized as null: %s", raw)
	}
	if parsed.NewestCreatedAt == nil || parsed.OldestCreatedAt == nil || *parsed.NewestCreatedAt <= *parsed.OldestCreatedAt {
		t.Fatalf("created range = %v..%v", parsed.OldestCreatedAt, parsed.NewestCreatedAt)
	}
	if !parsed.HasMore || parsed.NextCursor == nil || strings.Contains(*parsed.NextCursor, "X-CURSOR-1") {
		t.Fatalf("next_cursor should be opaque and present: %s", raw)
	}
	upstream, failure := decodeCursor(*parsed.NextCursor, ToolSearchPosts, aliceID, "go tui")
	if failure != nil || upstream != "X-CURSOR-1" {
		t.Fatalf("cursor round trip = %q, %v", upstream, failure)
	}
	// The text content mirrors the structured body, so clients that only read
	// text see the same contract.
	if len(result.Content) != 1 {
		t.Fatalf("content = %+v", result.Content)
	}
	if text, ok := result.Content[0].(*sdk.TextContent); !ok || !strings.Contains(text.Text, `"status":"ok"`) {
		t.Fatalf("text content = %+v", result.Content[0])
	}
}

func TestSearchWithNoMatchesIsEmptyNotAnError(t *testing.T) {
	fake := &fakeSession{search: map[string]page{"": {posts: nil, cursor: "X-END"}}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, raw := h.posts(ToolSearchPosts, map[string]any{"query": "zzz"})
	if result.IsError || parsed.Status != StatusEmpty || !parsed.Complete || parsed.Count != 0 || parsed.Error != nil {
		t.Fatalf("result = %s", raw)
	}
	if parsed.HasMore || parsed.NextCursor != nil {
		t.Fatalf("an empty page must not offer a next cursor: %s", raw)
	}
	if !strings.Contains(raw, `"posts":[]`) {
		t.Fatalf("posts must be an empty array, not null: %s", raw)
	}
}

func TestSecondPageFailureIsPartialWithTheFirstPageKept(t *testing.T) {
	reset := time.Now().Add(90 * time.Second)
	fake := &fakeSession{search: map[string]page{
		"":    {posts: []api.TimelinePost{post("1", "a", "one", true), post("2", "b", "two", true)}, cursor: "C1"},
		"C1":  {err: &api.RateLimitError{Reset: reset}},
		"bad": {},
	}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, raw := h.posts(ToolSearchPosts, map[string]any{"query": "q", "limit": 10})
	if result.IsError {
		t.Fatalf("partial results carry data and must not set isError: %s", raw)
	}
	if parsed.Status != StatusPartial || parsed.Complete || parsed.Count != 2 || parsed.PagesFetched != 1 {
		t.Fatalf("result = %s", raw)
	}
	if parsed.Error == nil || parsed.Error.Code != CodePartialResult || parsed.Error.Cause != CodeRateLimited {
		t.Fatalf("error = %+v", parsed.Error)
	}
	if parsed.Error.RetryAfterSeconds == nil || *parsed.Error.RetryAfterSeconds < 80 {
		t.Fatalf("retry_after_seconds = %v", parsed.Error.RetryAfterSeconds)
	}
	if parsed.NextCursor == nil {
		t.Fatalf("a partial result must offer the cursor to resume from: %s", raw)
	}
	if upstream, _ := decodeCursor(*parsed.NextCursor, ToolSearchPosts, aliceID, "q"); upstream != "C1" {
		t.Fatalf("resume cursor = %q, want C1", upstream)
	}
}

func TestFirstPageRateLimitIsAnError(t *testing.T) {
	fake := &fakeSession{search: map[string]page{"": {err: &api.RateLimitError{}}}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeRateLimited)
	if parsed.Complete || parsed.Count != 0 || parsed.NextCursor != nil {
		t.Fatalf("error result = %+v", parsed)
	}
}

func TestExpiredSessionIsAuthRequired(t *testing.T) {
	fake := &fakeSession{bookmarks: map[string]page{"": {err: fmt.Errorf("%w (HTTP 401)", api.ErrSessionExpired)}}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, _ := h.posts(ToolGetBookmarks, nil)
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAuthRequired)
}

func TestUpstreamShapeChangeIsReportedAsSuch(t *testing.T) {
	fake := &fakeSession{search: map[string]page{"": {err: fmt.Errorf("%w: x returned a malformed timeline response", api.ErrUpstreamChanged)}}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeUpstreamChanged)
}

func TestDiscoveryTimeoutIsATimeoutNotAShapeChange(t *testing.T) {
	wrapped := fmt.Errorf("%w: discover SearchTimeline endpoint: %w", api.ErrUpstreamChanged,
		&api.ConnectionError{Kind: "timeout", Err: context.DeadlineExceeded})
	fake := &fakeSession{search: map[string]page{"": {err: wrapped}}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeTimeout)
}

func TestCallTimeoutIsEnforcedAndClassified(t *testing.T) {
	fake := &fakeSession{block: true}
	h := newHarness(t, twoAccountStore(), fake, func(o *Options) { o.Timeout = 200 * time.Millisecond })
	started := time.Now()
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("call took %s; the timeout did not fire", elapsed)
	}
	assertFailure(t, result, parsed.Error, parsed.Status, CodeTimeout)

	// A per-call timeout_seconds above the cap is refused before any fetch.
	fake.reset()
	result, parsed, _ = h.posts(ToolSearchPosts, map[string]any{"query": "q", "timeout_seconds": 61})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeInvalidArgument)
	if fake.callCount() != 0 {
		t.Fatalf("invalid timeout still reached upstream: %v", fake.calls)
	}
}

func TestKeyringFailuresAreDistinguishedFromMissingSessions(t *testing.T) {
	store := twoAccountStore()
	store.loadErr[aliceID] = errors.New("reading auth_token:1000 from OS keyring: The name org.freedesktop.secrets was not provided by any .service files")
	h := newHarness(t, store, &fakeSession{}, nil)
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeKeyringUnavailable)

	store.loadErr[aliceID] = config.ErrSessionIncomplete
	result, parsed, _ = h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAuthRequired)
}

func TestAccountsOutsideTheAllowlistAreRefused(t *testing.T) {
	fake := &fakeSession{bookmarks: map[string]page{"": {posts: []api.TimelinePost{post("1", "a", "x", true)}}}}
	h := newHarness(t, twoAccountStore(), fake, nil)

	// bob is saved on this machine but not allowed for this server.
	result, parsed, _ := h.posts(ToolGetBookmarks, map[string]any{"account_id": "@bob"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAccountMismatch)
	result, parsed, _ = h.posts(ToolGetBookmarks, map[string]any{"account_id": bobID})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAccountMismatch)
	result, parsed, _ = h.posts(ToolGetBookmarks, map[string]any{"account_id": "@nobody"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAccountMismatch)
	if fake.callCount() != 0 {
		t.Fatalf("a refused account reached upstream: %v", fake.calls)
	}

	// Naming the allowed account by id or handle, in any case, works.
	for _, selector := range []string{aliceID, "@alice", "ALICE", " alice "} {
		_, parsed, raw := h.posts(ToolGetBookmarks, map[string]any{"account_id": selector})
		if parsed.Status != StatusOK || parsed.AccountID != aliceID {
			t.Fatalf("selector %q: %s", selector, raw)
		}
	}
}

func TestOmittedAccountIsRefusedWhenSeveralAreAllowed(t *testing.T) {
	fake := &fakeSession{bookmarks: map[string]page{"": {}}}
	h := newHarness(t, twoAccountStore(), fake, func(o *Options) { o.AllowedAccounts = []string{"@alice", bobID} })
	result, parsed, _ := h.posts(ToolGetBookmarks, nil)
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAccountRequired)
	if fake.callCount() != 0 {
		t.Fatalf("an ambiguous account fell back to something: %v", fake.calls)
	}
	_, parsed, raw := h.posts(ToolGetBookmarks, map[string]any{"account_id": bobID})
	if parsed.Status != StatusEmpty || parsed.AccountID != bobID {
		t.Fatalf("explicit bob = %s", raw)
	}
}

func TestAllowedAccountWithoutASavedSessionIsAuthRequired(t *testing.T) {
	store := twoAccountStore()
	h := newHarness(t, store, &fakeSession{}, func(o *Options) { o.AllowedAccounts = []string{"@erin"} })
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAuthRequired)

	store.version = 0
	store.accounts = nil
	result, parsed, _ = h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeAuthRequired)
	if !strings.Contains(parsed.Error.Message, "xeet auth") {
		t.Fatalf("message should say how to fix it: %q", parsed.Error.Message)
	}
}

func TestCursorsAreBoundToToolAccountAndQuery(t *testing.T) {
	fake := &fakeSession{
		search:    map[string]page{"": {posts: []api.TimelinePost{post("1", "a", "x", true)}, cursor: "S1"}, "S1": {posts: []api.TimelinePost{post("2", "b", "y", true)}}},
		bookmarks: map[string]page{"": {posts: []api.TimelinePost{post("3", "c", "z", true)}, cursor: "B1"}},
	}
	h := newHarness(t, twoAccountStore(), fake, nil)
	_, search, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q", "limit": 1})
	_, bookmarks, _ := h.posts(ToolGetBookmarks, map[string]any{"limit": 1})
	if search.NextCursor == nil || bookmarks.NextCursor == nil {
		t.Fatal("fixtures should produce cursors")
	}

	cases := map[string]map[string]any{
		"garbage":                 {"query": "q", "cursor": "not-a-cursor"},
		"raw upstream cursor":     {"query": "q", "cursor": "S1"},
		"bookmarks cursor":        {"query": "q", "cursor": *bookmarks.NextCursor},
		"other query":             {"query": "different", "cursor": *search.NextCursor},
		"cursor for another user": {"query": "q", "cursor": encodeCursor(ToolSearchPosts, bobID, "q", "S1")},
	}
	for name, args := range cases {
		fake.reset()
		result, parsed, _ := h.posts(ToolSearchPosts, args)
		assertFailure(t, result, parsed.Error, parsed.Status, CodeInvalidCursor)
		if fake.callCount() != 0 {
			t.Fatalf("%s: rejected cursor still reached upstream: %v", name, fake.calls)
		}
	}

	_, second, raw := h.posts(ToolSearchPosts, map[string]any{"query": "q", "cursor": *search.NextCursor})
	if second.Status != StatusOK || second.Count != 1 || second.Posts[0].ID != "2" {
		t.Fatalf("second page = %s", raw)
	}
}

func TestPageCapStopsCleanlyAndSaysSo(t *testing.T) {
	pages := map[string]page{}
	for i := 0; i < 5; i++ {
		var posts []api.TimelinePost
		for j := 0; j < 4; j++ {
			posts = append(posts, post(fmt.Sprintf("%d%d", i, j), "a", "t", true))
		}
		pages[fmt.Sprintf("P%d", i)] = page{posts: posts, cursor: fmt.Sprintf("P%d", i+1)}
	}
	pages[""] = pages["P0"]
	fake := &fakeSession{search: pages}
	h := newHarness(t, twoAccountStore(), fake, func(o *Options) { o.MaxPages = 2 })
	result, parsed, raw := h.posts(ToolSearchPosts, map[string]any{"query": "q", "limit": 100})
	if result.IsError || parsed.Status != StatusOK || !parsed.Complete || parsed.PagesFetched != 2 || parsed.Count != 8 {
		t.Fatalf("result = %s", raw)
	}
	if !parsed.HasMore || parsed.NextCursor == nil || !strings.Contains(parsed.Note, "page cap") {
		t.Fatalf("cap should leave a cursor and a note: %s", raw)
	}
}

func TestLimitDefaultsAndBounds(t *testing.T) {
	fake := &fakeSession{search: map[string]page{"": {posts: nil}}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	_, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	if parsed.RequestedLimit != defaultLimit {
		t.Fatalf("default limit = %d", parsed.RequestedLimit)
	}
	for _, limit := range []int{-1, 101, 1000} {
		result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "q", "limit": limit})
		assertFailure(t, result, parsed.Error, parsed.Status, CodeInvalidArgument)
	}
	for _, query := range []string{"", "   ", strings.Repeat("x", 513), "bad\x00query"} {
		result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": query})
		assertFailure(t, result, parsed.Error, parsed.Status, CodeInvalidArgument)
	}
	if fake.callCount() != 1 {
		t.Fatalf("invalid arguments reached upstream: %v", fake.calls)
	}
}

func TestSessionSecretsNeverAppearInResults(t *testing.T) {
	leaky := errors.New("timeline API error 400: request had Cookie: auth_token=" + aliceToken + "; ct0=" + aliceCT0)
	fake := &fakeSession{
		search:    map[string]page{"": {err: leaky}},
		viewerErr: leaky,
	}
	h := newHarness(t, twoAccountStore(), fake, nil)
	_, parsed, raw := h.posts(ToolSearchPosts, map[string]any{"query": "q"})
	if parsed.Status != StatusError {
		t.Fatalf("result = %s", raw)
	}
	for _, secret := range []string{aliceToken, aliceCT0} {
		if strings.Contains(raw, secret) {
			t.Fatalf("result leaks %q: %s", secret, raw)
		}
	}
	if !strings.Contains(raw, "[redacted]") {
		t.Fatalf("expected redaction marker: %s", raw)
	}
	_, _, healthRaw := h.health(nil)
	for _, secret := range []string{aliceToken, aliceCT0} {
		if strings.Contains(healthRaw, secret) {
			t.Fatalf("health leaks %q: %s", secret, healthRaw)
		}
	}
}

func TestHealthReportsAnAuthenticatedMatchingSession(t *testing.T) {
	fake := &fakeSession{viewer: &api.Account{ID: aliceID, Handle: aliceHandle, Name: "Alice"}}
	h := newHarness(t, twoAccountStore(), fake, nil)
	result, parsed, raw := h.health(nil)
	if result.IsError || parsed.Status != StatusOK {
		t.Fatalf("result = %s", raw)
	}
	if !parsed.Configured || parsed.ConfigVersion != 2 || parsed.Keyring != keyringOK || parsed.Connection != connectionAuthenticated {
		t.Fatalf("result = %s", raw)
	}
	if parsed.AccountID != aliceID || parsed.Viewer == nil || parsed.Viewer.Handle != aliceHandle || parsed.AccountMatch == nil || !*parsed.AccountMatch {
		t.Fatalf("identity = %s", raw)
	}
	if parsed.SessionSource != "Chrome / Default" || parsed.CookieExpiresAt == nil || parsed.ImportedAt == nil || parsed.CookieExpiredLocal == nil || *parsed.CookieExpiredLocal {
		t.Fatalf("session metadata = %s", raw)
	}
	if parsed.CheckedAt == nil || parsed.LastAuthenticatedAt == nil || *parsed.CheckedAt != h.now.Format(time.RFC3339) {
		t.Fatalf("timestamps = %s", raw)
	}
	if fake.callCount() != 1 || fake.calls[0] != "viewer" {
		t.Fatalf("health must make exactly one lightweight read, got %v", fake.calls)
	}
}

func TestHealthDistinguishesFailureClasses(t *testing.T) {
	t.Run("session belongs to another account", func(t *testing.T) {
		fake := &fakeSession{viewer: &api.Account{ID: "9999", Handle: "someone"}}
		h := newHarness(t, twoAccountStore(), fake, nil)
		result, parsed, _ := h.health(nil)
		assertFailure(t, result, parsed.Error, parsed.Status, CodeAccountMismatch)
		if parsed.Connection != connectionAuthenticated || parsed.AccountMatch == nil || *parsed.AccountMatch {
			t.Fatalf("result = %+v", parsed)
		}
	})
	t.Run("cookies rejected", func(t *testing.T) {
		fake := &fakeSession{viewerErr: api.ErrSessionExpired}
		h := newHarness(t, twoAccountStore(), fake, nil)
		result, parsed, _ := h.health(nil)
		assertFailure(t, result, parsed.Error, parsed.Status, CodeAuthRequired)
		if parsed.Connection != connectionAuthRequired || parsed.Keyring != keyringOK || parsed.LastAuthenticatedAt != nil {
			t.Fatalf("result = %+v", parsed)
		}
	})
	t.Run("keyring unavailable", func(t *testing.T) {
		store := twoAccountStore()
		store.loadErr[aliceID] = errors.New("reading auth_token:1000 from OS keyring: no dbus session")
		h := newHarness(t, store, &fakeSession{}, nil)
		result, parsed, _ := h.health(nil)
		assertFailure(t, result, parsed.Error, parsed.Status, CodeKeyringUnavailable)
		if parsed.Keyring != keyringUnavailable || parsed.Connection != connectionNotChecked {
			t.Fatalf("result = %+v", parsed)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		fake := &fakeSession{block: true}
		h := newHarness(t, twoAccountStore(), fake, func(o *Options) { o.HealthTimeout = 100 * time.Millisecond })
		result, parsed, _ := h.health(nil)
		assertFailure(t, result, parsed.Error, parsed.Status, CodeTimeout)
		if parsed.Connection != connectionTimeout {
			t.Fatalf("result = %+v", parsed)
		}
	})
	t.Run("rate limited", func(t *testing.T) {
		fake := &fakeSession{viewerErr: &api.RateLimitError{}}
		h := newHarness(t, twoAccountStore(), fake, nil)
		result, parsed, _ := h.health(nil)
		assertFailure(t, result, parsed.Error, parsed.Status, CodeRateLimited)
		if parsed.Connection != connectionRateLimited {
			t.Fatalf("result = %+v", parsed)
		}
	})
}

func TestConcurrencyCapReturnsBusyInsteadOfQueueingForever(t *testing.T) {
	fake := &fakeSession{block: true}
	h := newHarness(t, twoAccountStore(), fake, func(o *Options) { o.Timeout = 8 * time.Second; o.MaxConcurrent = 1 })
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.posts(ToolSearchPosts, map[string]any{"query": "slow"})
	}()
	// Let the first call take the only slot before the second arrives.
	deadline := time.Now().Add(2 * time.Second)
	for fake.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	result, parsed, _ := h.posts(ToolSearchPosts, map[string]any{"query": "second"})
	assertFailure(t, result, parsed.Error, parsed.Status, CodeBusy)
	<-done
}

func TestNewRefusesAnEmptyAllowlistAndOutOfRangeLimits(t *testing.T) {
	base := Options{Store: twoAccountStore(), NewSession: func(*config.Config) Session { return &fakeSession{} }}
	if _, err := New(base); err == nil {
		t.Fatal("New accepted an empty allowlist")
	}
	withAccount := base
	withAccount.AllowedAccounts = []string{"@alice"}
	tooLong := withAccount
	tooLong.Timeout = 61 * time.Second
	if _, err := New(tooLong); err == nil {
		t.Fatal("New accepted a timeout above the cap")
	}
	tooMany := withAccount
	tooMany.MaxPages = 11
	if _, err := New(tooMany); err == nil {
		t.Fatal("New accepted a page cap above the maximum")
	}
	if _, err := New(withAccount); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyMapsTheAPIErrorFamilies(t *testing.T) {
	cases := []struct {
		err  error
		code Code
	}{
		{&api.RateLimitError{}, CodeRateLimited},
		{context.DeadlineExceeded, CodeTimeout},
		{&api.ConnectionError{Kind: "offline", Err: errors.New("dns")}, CodeNetworkError},
		{&api.ConnectionError{Kind: "timeout", Err: errors.New("t")}, CodeTimeout},
		{fmt.Errorf("wrapped: %w", api.ErrSessionExpired), CodeAuthRequired},
		{config.ErrSessionIncomplete, CodeAuthRequired},
		{fmt.Errorf("%w: parse", api.ErrUpstreamChanged), CodeUpstreamChanged},
		{&api.ServiceUnavailableError{Status: 503}, CodeUpstreamUnavailable},
		{errors.New("timeline API error 400: nope"), CodeUpstreamError},
	}
	for _, c := range cases {
		if got := classify(c.err); got.Code != c.code {
			t.Errorf("classify(%v) = %s, want %s", c.err, got.Code, c.code)
		}
	}
	if got := classify(nil); got != nil {
		t.Errorf("classify(nil) = %+v", got)
	}
}
