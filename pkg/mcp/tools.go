package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/melqtx/xeet/pkg/api"
	"github.com/melqtx/xeet/pkg/config"
)

// Tool names are part of the contract clients pin their allowlists to.
const (
	ToolSearchPosts   = "search_x_posts"
	ToolGetBookmarks  = "get_x_bookmarks"
	ToolSessionHealth = "get_x_session_health"
)

const (
	defaultLimit       = 20
	maxLimit           = 100
	maxQueryLength     = 512
	maxAccountIDLength = 64
	minTimeoutSeconds  = 1
	maxTimeoutSeconds  = 60
	maxUpstreamCount   = 100
)

const (
	StatusOK      = "ok"
	StatusEmpty   = "empty"
	StatusPartial = "partial"
	StatusError   = "error"
)

type SearchInput struct {
	Query          string `json:"query" jsonschema:"X search query, as typed into the x.com search box (operators such as from:, since:, until:, min_faves: work). 1-512 characters."`
	AccountID      string `json:"account_id,omitempty" jsonschema:"Saved account to search as: a numeric X user id or @handle. Must be one the server allows. Optional only when the server allows exactly one account."`
	Limit          int    `json:"limit,omitempty" jsonschema:"Maximum posts to return, 1-100. Default 20."`
	Cursor         string `json:"cursor,omitempty" jsonschema:"next_cursor from a previous search_x_posts result with the same query and account. Cursors issued for another query, account, or tool are rejected."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Upper bound for the whole call, 1-60 seconds. Default is the server's configured timeout (30s)."`
}

type BookmarksInput struct {
	AccountID      string `json:"account_id,omitempty" jsonschema:"Saved account whose bookmarks to read: a numeric X user id or @handle. Must be one the server allows. Optional only when the server allows exactly one account."`
	Limit          int    `json:"limit,omitempty" jsonschema:"Maximum bookmarks to return, 1-100. Default 20."`
	Cursor         string `json:"cursor,omitempty" jsonschema:"next_cursor from a previous get_x_bookmarks result for the same account."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Upper bound for the whole call, 1-60 seconds. Default is the server's configured timeout (30s)."`
}

type HealthInput struct {
	AccountID      string `json:"account_id,omitempty" jsonschema:"Saved account to check: a numeric X user id or @handle. Optional only when the server allows exactly one account."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Upper bound for the live check, 1-60 seconds. Default 15."`
}

type Author struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
	URL    string `json:"url"`
}

// Metrics are null when X omitted the counter, which is different from a
// counter that reads zero.
type Metrics struct {
	Replies *int   `json:"replies"`
	Reposts *int   `json:"reposts"`
	Likes   *int   `json:"likes"`
	Views   *int64 `json:"views"`
}

type Post struct {
	ID          string  `json:"id"`
	URL         string  `json:"url"`
	Text        string  `json:"text"`
	Author      Author  `json:"author"`
	CreatedAt   *string `json:"created_at"`
	Metrics     Metrics `json:"metrics"`
	MediaCount  int     `json:"media_count"`
	InReplyToID string  `json:"in_reply_to_id,omitempty"`
}

// PostsResult is the shared output of search_x_posts and get_x_bookmarks.
//
// complete means every page this call attempted was fetched without error.
// has_more / next_cursor describe whether X has a further page; they say
// nothing about failure. A partial result therefore has complete=false,
// error.code=PARTIAL_RESULT, and next_cursor pointing after the last page
// that did succeed.
type PostsResult struct {
	Status          string   `json:"status"`
	Tool            string   `json:"tool"`
	Source          string   `json:"source"`
	Freshness       string   `json:"freshness"`
	AccountID       string   `json:"account_id"`
	AccountHandle   string   `json:"account_handle,omitempty"`
	Query           string   `json:"query,omitempty"`
	FetchedAt       string   `json:"fetched_at"`
	RequestedLimit  int      `json:"requested_limit"`
	Count           int      `json:"count"`
	PagesFetched    int      `json:"pages_fetched"`
	Complete        bool     `json:"complete"`
	HasMore         bool     `json:"has_more"`
	NextCursor      *string  `json:"next_cursor"`
	NewestCreatedAt *string  `json:"newest_created_at"`
	OldestCreatedAt *string  `json:"oldest_created_at"`
	Posts           []Post   `json:"posts"`
	Note            string   `json:"note,omitempty"`
	Error           *Failure `json:"error,omitempty"`
}

type Viewer struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

// HealthResult reports the saved session for one account without changing
// anything: no login, no cookie import, no browser. checked_at is when this
// call contacted X; last_authenticated_at is the most recent successful
// check in this server process, or null since start.
type HealthResult struct {
	Status              string   `json:"status"`
	Tool                string   `json:"tool"`
	FetchedAt           string   `json:"fetched_at"`
	AccountID           string   `json:"account_id,omitempty"`
	AccountHandle       string   `json:"account_handle,omitempty"`
	AllowedAccounts     []string `json:"allowed_accounts"`
	Configured          bool     `json:"configured"`
	ConfigVersion       int      `json:"config_version"`
	Keyring             string   `json:"keyring"`
	SessionSource       string   `json:"session_source,omitempty"`
	CookieExpiresAt     *string  `json:"cookie_expires_at"`
	CookieExpiredLocal  *bool    `json:"cookie_expired_locally"`
	ImportedAt          *string  `json:"imported_at"`
	Connection          string   `json:"connection"`
	CheckedAt           *string  `json:"checked_at"`
	LastAuthenticatedAt *string  `json:"last_authenticated_at"`
	Viewer              *Viewer  `json:"viewer"`
	AccountMatch        *bool    `json:"account_match"`
	Error               *Failure `json:"error,omitempty"`
}

const (
	keyringOK          = "ok"
	keyringUnavailable = "unavailable"
	keyringIncomplete  = "incomplete"
	keyringNotChecked  = "not_checked"

	connectionAuthenticated = "authenticated"
	connectionAuthRequired  = "auth_required"
	connectionRateLimited   = "rate_limited"
	connectionTimeout       = "timeout"
	connectionUnreachable   = "unreachable"
	connectionChanged       = "upstream_changed"
	connectionError         = "upstream_error"
	connectionNotChecked    = "not_checked"
)

func validateQuery(query string) *Failure {
	if strings.TrimSpace(query) == "" {
		return failf(CodeInvalidArgument, "query must not be empty")
	}
	if len(query) > maxQueryLength {
		return failf(CodeInvalidArgument, fmt.Sprintf("query must be at most %d bytes", maxQueryLength))
	}
	for _, r := range query {
		if unicode.IsControl(r) && r != '\t' {
			return failf(CodeInvalidArgument, "query must not contain control characters")
		}
	}
	return nil
}

func validateLimit(limit int) (int, *Failure) {
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 1 || limit > maxLimit {
		return 0, failf(CodeInvalidArgument, fmt.Sprintf("limit must be between 1 and %d", maxLimit))
	}
	return limit, nil
}

func (s *Server) callTimeout(seconds int, fallback time.Duration) (time.Duration, *Failure) {
	if seconds == 0 {
		return fallback, nil
	}
	if seconds < minTimeoutSeconds || seconds > maxTimeoutSeconds {
		return 0, failf(CodeInvalidArgument, fmt.Sprintf("timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds))
	}
	return time.Duration(seconds) * time.Second, nil
}

func postURL(p api.TimelinePost) string {
	if p.ID == "" {
		return ""
	}
	handle := p.Handle
	if handle == "" {
		handle = "i"
	}
	return "https://x.com/" + handle + "/status/" + p.ID
}

func convertPost(p api.TimelinePost) Post {
	post := Post{
		ID:          p.ID,
		URL:         postURL(p),
		Text:        p.Text,
		Author:      Author{Name: p.AuthorName, Handle: p.Handle},
		MediaCount:  p.MediaCount,
		InReplyToID: p.InReplyToID,
	}
	if p.Handle != "" {
		post.Author.URL = "https://x.com/" + p.Handle
	}
	if !p.CreatedAt.IsZero() {
		created := p.CreatedAt.UTC().Format(time.RFC3339)
		post.CreatedAt = &created
	}
	if p.EngagementKnown {
		replies, reposts, likes := p.ReplyCount, p.RepostCount, p.LikeCount
		post.Metrics = Metrics{Replies: &replies, Reposts: &reposts, Likes: &likes}
	}
	if views, err := strconv.ParseInt(p.ViewCount, 10, 64); err == nil {
		post.Metrics.Views = &views
	}
	return post
}

func timeString(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func sessionSource(cfg *config.Config) string {
	if cfg == nil || cfg.SessionBrowser == "" {
		return ""
	}
	if cfg.SessionProfile != "" {
		return cfg.SessionBrowser + " / " + cfg.SessionProfile
	}
	return cfg.SessionBrowser
}

// fetchPosts walks pages until limit is met, X runs out, or the page cap is
// hit. A failure on the first page is the call's failure; a failure on a later
// page keeps what was fetched and reports it as partial, with the cursor of
// the last page that succeeded so the caller can resume.
func (s *Server) fetchPosts(ctx context.Context, fetch func(ctx context.Context, cursor string, count int) (*api.TimelinePage, error), upstream string, limit int) (posts []Post, next string, pages int, note string, failure *Failure) {
	seen := map[string]bool{}
	cursor := upstream
	for pages < s.maxPages {
		if pages > 0 && s.pageDelay > 0 {
			select {
			case <-ctx.Done():
				return posts, cursor, pages, note, partialFailure(classify(ctx.Err()), pages)
			case <-time.After(s.pageDelay):
			}
		}
		count := limit - len(posts)
		if count > maxUpstreamCount {
			count = maxUpstreamCount
		}
		page, err := fetch(ctx, cursor, count)
		if err != nil {
			return posts, cursor, pages, note, partialFailure(classify(err), pages)
		}
		pages++
		added, skipped := 0, 0
		for _, raw := range page.Posts {
			if seen[raw.ID] {
				continue
			}
			seen[raw.ID] = true
			added++
			if len(posts) >= limit {
				skipped++
				continue
			}
			posts = append(posts, convertPost(raw))
		}
		if skipped > 0 {
			// X sizes search pages itself, so a page can overshoot limit. The
			// overshoot is dropped and next_cursor resumes after the page, which
			// is lossy; saying so beats letting those posts vanish silently.
			note = fmt.Sprintf("%d post(s) on the last page were beyond limit and skipped; raise limit or re-run without a cursor to see them", skipped)
		}
		if len(posts) >= limit {
			return posts, page.Cursor, pages, note, nil
		}
		if page.Cursor == "" || added == 0 {
			return posts, "", pages, note, nil
		}
		cursor = page.Cursor
	}
	note = joinNotes(note, fmt.Sprintf("stopped after the per-call page cap of %d pages; continue with next_cursor", s.maxPages))
	return posts, cursor, pages, note, nil
}

func joinNotes(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return existing + "; " + extra
}

// partialFailure keeps the underlying class visible as cause so a caller can
// tell a rate-limited second page from a timed-out one.
func partialFailure(failure *Failure, successfulPages int) *Failure {
	if successfulPages == 0 {
		return failure
	}
	return &Failure{
		Code:              CodePartialResult,
		Message:           fmt.Sprintf("page %d failed after %d page(s) succeeded: %s", successfulPages+1, successfulPages, failure.Message),
		Cause:             failure.Code,
		RetryAfterSeconds: failure.RetryAfterSeconds,
	}
}

// postsArgs is what search and bookmarks share once the query is peeled off.
type postsArgs struct {
	AccountID      string
	Limit          int
	Cursor         string
	TimeoutSeconds int
}

func (s *Server) runPosts(ctx context.Context, tool, source, query string, in postsArgs) PostsResult {
	result := PostsResult{
		Tool:           tool,
		Source:         source,
		Freshness:      "live",
		Query:          query,
		FetchedAt:      s.now().UTC().Format(time.RFC3339),
		RequestedLimit: in.Limit,
		Posts:          []Post{},
	}
	fail := func(f *Failure) PostsResult {
		result.Status = StatusError
		result.Error = f
		return result
	}

	if tool == ToolSearchPosts {
		if f := validateQuery(query); f != nil {
			return fail(f)
		}
	}
	limit, f := validateLimit(in.Limit)
	if f != nil {
		return fail(f)
	}
	result.RequestedLimit = limit
	timeout, f := s.callTimeout(in.TimeoutSeconds, s.timeout)
	if f != nil {
		return fail(f)
	}
	acct, f := s.resolveAccount(in.AccountID)
	if f != nil {
		return fail(f)
	}
	result.AccountID = acct.UserID
	result.AccountHandle = acct.Handle
	upstream, f := decodeCursor(in.Cursor, tool, acct.UserID, query)
	if f != nil {
		return fail(f)
	}

	release, f := s.acquire(ctx)
	if f != nil {
		return fail(f)
	}
	defer release()

	session, _, f := s.openSession(acct)
	if f != nil {
		return fail(f)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fetch := func(ctx context.Context, cursor string, count int) (*api.TimelinePage, error) {
		if tool == ToolSearchPosts {
			return session.FetchSearchTimeline(ctx, query, cursor, count)
		}
		return session.FetchBookmarks(ctx, cursor, count)
	}
	posts, next, pages, note, failure := s.fetchPosts(ctx, fetch, upstream, limit)
	if posts == nil {
		posts = []Post{}
	}
	result.Posts = posts
	result.Count = len(posts)
	result.PagesFetched = pages
	result.Note = note
	if cursor := encodeCursor(tool, acct.UserID, query, next); cursor != "" {
		result.NextCursor = &cursor
		result.HasMore = true
	}
	result.NewestCreatedAt, result.OldestCreatedAt = createdRange(posts)

	switch {
	case failure != nil && failure.Code == CodePartialResult:
		result.Status = StatusPartial
		result.Complete = false
		result.Error = failure
	case failure != nil:
		return fail(failure)
	case len(posts) == 0:
		result.Status = StatusEmpty
		result.Complete = true
	default:
		result.Status = StatusOK
		result.Complete = true
	}
	return result
}

func createdRange(posts []Post) (newest, oldest *string) {
	var newestAt, oldestAt time.Time
	for _, post := range posts {
		if post.CreatedAt == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, *post.CreatedAt)
		if err != nil {
			continue
		}
		if newestAt.IsZero() || at.After(newestAt) {
			newestAt = at
		}
		if oldestAt.IsZero() || at.Before(oldestAt) {
			oldestAt = at
		}
	}
	return timeString(newestAt), timeString(oldestAt)
}

func (s *Server) runHealth(ctx context.Context, in HealthInput) HealthResult {
	result := HealthResult{
		Tool:            ToolSessionHealth,
		FetchedAt:       s.now().UTC().Format(time.RFC3339),
		AllowedAccounts: append([]string{}, s.allowed...),
		Keyring:         keyringNotChecked,
		Connection:      connectionNotChecked,
	}
	if version, err := s.store.Version(); err == nil {
		result.ConfigVersion = version
		result.Configured = version > 0
	}
	if last := s.lastAuthenticated(); !last.IsZero() {
		result.LastAuthenticatedAt = timeString(last)
	}
	fail := func(f *Failure) HealthResult {
		result.Status = StatusError
		result.Error = f
		return result
	}

	timeout, f := s.callTimeout(in.TimeoutSeconds, s.healthTimeout)
	if f != nil {
		return fail(f)
	}
	acct, f := s.resolveAccount(in.AccountID)
	if f != nil {
		return fail(f)
	}
	result.AccountID = acct.UserID
	result.AccountHandle = acct.Handle

	release, f := s.acquire(ctx)
	if f != nil {
		return fail(f)
	}
	defer release()

	session, cfg, f := s.openSession(acct)
	if f != nil {
		switch f.Code {
		case CodeKeyringUnavailable:
			result.Keyring = keyringUnavailable
		default:
			result.Keyring = keyringIncomplete
		}
		return fail(f)
	}
	result.Keyring = keyringOK
	result.SessionSource = sessionSource(cfg)
	result.CookieExpiresAt = timeString(cfg.SessionExpires)
	if !cfg.SessionExpires.IsZero() {
		expired := s.now().After(cfg.SessionExpires)
		result.CookieExpiredLocal = &expired
	}
	result.ImportedAt = timeString(cfg.SessionImported)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	checkedAt := s.now()
	result.CheckedAt = timeString(checkedAt)
	viewer, err := session.FetchViewer(ctx)
	if err != nil {
		failure := classify(err)
		result.Connection = connectionFor(failure.Code)
		return fail(failure)
	}
	result.Connection = connectionAuthenticated
	result.Viewer = &Viewer{ID: viewer.ID, Handle: viewer.Handle, Name: viewer.Name}
	match := viewer.ID != "" && viewer.ID == acct.UserID
	result.AccountMatch = &match
	s.recordAuthenticated(checkedAt)
	result.LastAuthenticatedAt = timeString(checkedAt)
	if !match {
		return fail(failf(CodeAccountMismatch,
			fmt.Sprintf("the saved session for %s belongs to a different X account (@%s); re-run 'xeet auth'", acct.UserID, viewer.Handle)))
	}
	result.Status = StatusOK
	return result
}

func connectionFor(code Code) string {
	switch code {
	case CodeAuthRequired:
		return connectionAuthRequired
	case CodeRateLimited:
		return connectionRateLimited
	case CodeTimeout:
		return connectionTimeout
	case CodeNetworkError, CodeUpstreamUnavailable:
		return connectionUnreachable
	case CodeUpstreamChanged:
		return connectionChanged
	}
	return connectionError
}
