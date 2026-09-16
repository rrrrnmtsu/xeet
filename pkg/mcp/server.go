// Package mcp exposes xeet's read-only X lookups (post search, bookmarks,
// session health) as a Model Context Protocol server over stdio.
//
// The package is the whole allowlist: it constructs the server, registers
// exactly three tools, and owns the only path from a tool call to the X web
// client. Posting, liking, bookmarking, and following live in pkg/api too, but
// nothing here can reach them, so tools/list is the security boundary rather
// than a hint.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/melqtx/xeet/pkg/api"
	"github.com/melqtx/xeet/pkg/config"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultTimeout       = 30 * time.Second
	DefaultHealthTimeout = 15 * time.Second
	MaxTimeout           = 60 * time.Second
	DefaultMaxPages      = 3
	MaxMaxPages          = 10
	DefaultPageDelay     = time.Second
	DefaultMaxConcurrent = 1
	acquireWait          = 5 * time.Second
)

// Options configure a Server. Zero values take the documented defaults; the
// only required field is AllowedAccounts, because a server that can read any
// saved account is the implicit-fallback the tools refuse to have.
type Options struct {
	// Store reads saved accounts. Defaults to the user's ~/.xeet.yaml and OS
	// keyring via config.NewConfigManager.
	Store Store
	// NewSession builds the X client for one account. Defaults to
	// api.NewWebClient; tests inject fakes.
	NewSession func(*config.Config) Session
	// AllowedAccounts are the handles or user ids tools may read as.
	AllowedAccounts []string
	// Timeout bounds one search or bookmarks call; capped at MaxTimeout.
	Timeout time.Duration
	// HealthTimeout bounds the live check in get_x_session_health.
	HealthTimeout time.Duration
	// MaxPages caps upstream pages per call; capped at MaxMaxPages.
	MaxPages int
	// PageDelay is the pause between upstream pages within one call.
	PageDelay time.Duration
	// MaxConcurrent caps tool calls in flight; extra callers wait briefly and
	// then get BUSY.
	MaxConcurrent int
	// Logger receives structured diagnostics. It must never write to stdout,
	// which carries the protocol. Defaults to a JSON logger on Stderr.
	Logger *slog.Logger
	// Stderr is where the default logger writes. Defaults to os.Stderr.
	Stderr io.Writer
	// Version is reported to clients as the server version.
	Version string
	// Now is the clock; tests freeze it.
	Now func() time.Time
}

// Server is one configured MCP server. It is safe for concurrent use.
type Server struct {
	store         Store
	newSession    func(*config.Config) Session
	allowed       []string
	timeout       time.Duration
	healthTimeout time.Duration
	maxPages      int
	pageDelay     time.Duration
	sem           chan struct{}
	logger        *slog.Logger
	now           func() time.Time
	cache         sessionCache
	mcp           *sdk.Server

	mu         sync.Mutex
	lastAuthAt time.Time
}

// New validates opts and builds a server with the three read tools
// registered. It does not touch the keyring or the network.
func New(opts Options) (*Server, error) {
	allowed := make([]string, 0, len(opts.AllowedAccounts))
	for _, entry := range opts.AllowedAccounts {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if len(entry) > maxAccountIDLength {
			return nil, fmt.Errorf("allowed account %q is too long", entry)
		}
		allowed = append(allowed, entry)
	}
	if len(allowed) == 0 {
		return nil, errors.New("at least one allowed account is required (--allow-account <handle|user-id>)")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		return nil, fmt.Errorf("timeout %s exceeds the maximum of %s", timeout, MaxTimeout)
	}
	healthTimeout := opts.HealthTimeout
	if healthTimeout <= 0 {
		healthTimeout = DefaultHealthTimeout
	}
	if healthTimeout > MaxTimeout {
		return nil, fmt.Errorf("health timeout %s exceeds the maximum of %s", healthTimeout, MaxTimeout)
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	if maxPages > MaxMaxPages {
		return nil, fmt.Errorf("max pages %d exceeds the maximum of %d", maxPages, MaxMaxPages)
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	pageDelay := opts.PageDelay
	if pageDelay < 0 {
		pageDelay = 0
	}

	store := opts.Store
	if store == nil {
		manager, err := config.NewConfigManager()
		if err != nil {
			return nil, err
		}
		store = manager
	}
	newSession := opts.NewSession
	if newSession == nil {
		newSession = func(cfg *config.Config) Session { return api.NewWebClient(cfg) }
	}
	logger := opts.Logger
	if logger == nil {
		stderr := opts.Stderr
		if stderr == nil {
			stderr = os.Stderr
		}
		logger = slog.New(slog.NewJSONHandler(stderr, nil))
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}

	s := &Server{
		store:         store,
		newSession:    newSession,
		allowed:       allowed,
		timeout:       timeout,
		healthTimeout: healthTimeout,
		maxPages:      maxPages,
		pageDelay:     pageDelay,
		sem:           make(chan struct{}, maxConcurrent),
		logger:        logger,
		now:           now,
		cache:         sessionCache{sessions: map[string]cachedSession{}},
	}
	s.mcp = sdk.NewServer(&sdk.Implementation{Name: "xeet", Title: "xeet (X search and bookmarks, read-only)", Version: version}, &sdk.ServerOptions{
		Instructions: "Read-only access to X (Twitter) through a saved browser session: search posts, list the account's bookmarks, and check that the session still works. " +
			"Nothing here posts, likes, bookmarks, follows, or sends messages. " +
			"Results carry status (ok, empty, partial, error), fetched_at, and a next_cursor when X has more; " +
			"complete=false with error.code=PARTIAL_RESULT means a later page failed and the posts returned are only what was fetched before that.",
		Logger: logger,
	})
	s.register()
	return s, nil
}

// MCP returns the underlying protocol server, for connecting a transport other
// than stdio (tests use in-memory pipes).
func (s *Server) MCP() *sdk.Server { return s.mcp }

// Run serves MCP over stdin/stdout until the client disconnects or ctx ends.
func (s *Server) Run(ctx context.Context) error {
	return s.mcp.Run(ctx, &sdk.StdioTransport{})
}

// ToolNames is the complete set of tools this server registers, in order.
func ToolNames() []string {
	return []string{ToolSearchPosts, ToolGetBookmarks, ToolSessionHealth}
}

func readOnly() *sdk.ToolAnnotations {
	falseValue := false
	trueValue := true
	return &sdk.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &falseValue,
		IdempotentHint:  true,
		OpenWorldHint:   &trueValue,
	}
}

func (s *Server) register() {
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name:  ToolSearchPosts,
		Title: "Search X posts",
		Description: "Search X (Twitter) posts through the saved browser session and return the matching posts: id, URL, text, author, created_at, and reaction counts (null when X did not include them). " +
			"Results are X's 'Top' ranking for the query, fetched live at call time. Use next_cursor with the same query to page.",
		Annotations: readOnly(),
	}, s.handleSearch)
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name:  ToolGetBookmarks,
		Title: "Get X bookmarks",
		Description: "List the posts the allowed X account has bookmarked, newest first, fetched live from x.com (not from a local database). " +
			"Returns id, URL, text, author, created_at, and reaction counts. Use next_cursor to page.",
		Annotations: readOnly(),
	}, s.handleBookmarks)
	sdk.AddTool(s.mcp, &sdk.Tool{
		Name:  ToolSessionHealth,
		Title: "Check X session health",
		Description: "Report whether the saved X session for an allowed account is configured, readable from the OS keyring, and still accepted by x.com. " +
			"Makes one lightweight authenticated read; never logs in, imports cookies, or opens a browser.",
		Annotations: readOnly(),
	}, s.handleHealth)
}

func (s *Server) handleSearch(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, PostsResult, error) {
	started := s.now()
	result := s.runPosts(ctx, ToolSearchPosts, "x.com SearchTimeline (live, product=Top)", in.Query, postsArgs{
		AccountID: in.AccountID, Limit: in.Limit, Cursor: in.Cursor, TimeoutSeconds: in.TimeoutSeconds,
	})
	s.logPosts(result, started)
	return toolResult(result.Status, result), result, nil
}

func (s *Server) handleBookmarks(ctx context.Context, _ *sdk.CallToolRequest, in BookmarksInput) (*sdk.CallToolResult, PostsResult, error) {
	started := s.now()
	result := s.runPosts(ctx, ToolGetBookmarks, "x.com Bookmarks (live)", "", postsArgs(in))
	s.logPosts(result, started)
	return toolResult(result.Status, result), result, nil
}

func (s *Server) handleHealth(ctx context.Context, _ *sdk.CallToolRequest, in HealthInput) (*sdk.CallToolResult, HealthResult, error) {
	started := s.now()
	result := s.runHealth(ctx, in)
	attrs := []any{"tool", result.Tool, "status", result.Status, "account", result.AccountID,
		"keyring", result.Keyring, "connection", result.Connection, "duration_ms", s.now().Sub(started).Milliseconds()}
	if result.Error != nil {
		attrs = append(attrs, "code", result.Error.Code)
	}
	s.logger.Info("tool call", attrs...)
	return toolResult(result.Status, result), result, nil
}

// toolResult marks failed calls with isError so clients that only look at the
// flag see them, while the structured body still carries the failure detail.
// The SDK fills Content and StructuredContent from the typed output.
func toolResult(status string, output any) *sdk.CallToolResult {
	result := &sdk.CallToolResult{IsError: status == StatusError}
	if text, err := json.Marshal(output); err == nil {
		result.Content = []sdk.Content{&sdk.TextContent{Text: string(text)}}
	}
	return result
}

func (s *Server) logPosts(result PostsResult, started time.Time) {
	attrs := []any{"tool", result.Tool, "status", result.Status, "account", result.AccountID,
		"count", result.Count, "pages", result.PagesFetched, "complete", result.Complete,
		"has_more", result.HasMore, "duration_ms", s.now().Sub(started).Milliseconds()}
	if result.Error != nil {
		attrs = append(attrs, "code", result.Error.Code)
		if result.Error.Cause != "" {
			attrs = append(attrs, "cause", result.Error.Cause)
		}
	}
	s.logger.Info("tool call", attrs...)
}

// acquire takes a concurrency slot, waiting briefly rather than queueing calls
// behind a slow one indefinitely.
func (s *Server) acquire(ctx context.Context) (func(), *Failure) {
	wait := time.NewTimer(acquireWait)
	defer wait.Stop()
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, failf(CodeTimeout, "the call was cancelled while waiting for a slot")
	case <-wait.C:
		return nil, failf(CodeBusy, "another call is in progress; retry shortly")
	}
}

func (s *Server) lastAuthenticated() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuthAt
}

func (s *Server) recordAuthenticated(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.lastAuthAt) {
		s.lastAuthAt = at
	}
}
