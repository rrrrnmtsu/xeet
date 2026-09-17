package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/melqtx/xeet/pkg/api"
	"github.com/melqtx/xeet/pkg/config"
)

// Session is the slice of the X web client the tools use. It is the whole of
// what the server can do to X: three reads, no mutation entry point exists to
// register by accident.
type Session interface {
	FetchSearchTimeline(ctx context.Context, query, cursor string, count int) (*api.TimelinePage, error)
	FetchBookmarks(ctx context.Context, cursor string, count int) (*api.TimelinePage, error)
	FetchViewer(ctx context.Context) (*api.Account, error)
}

// WriteSession is the mutation half, kept separate from Session so a
// read-only server literally cannot reach it: the write tools type-assert for
// this interface and refuse when the session does not implement it. The set is
// deliberately two calls. Reposting, quoting, bookmarking, following and
// direct messages are not here and cannot be reached from MCP.
type WriteSession interface {
	Session
	PostTweet(ctx context.Context, text, replyToID string, uploads []api.Upload, progress api.ProgressFunc) (string, error)
	SetTweetLiked(ctx context.Context, tweetID string, liked bool) error
}

// Store is the slice of config.ConfigManager the server reads. None of these
// write: the server never persists rediscovered query ids or viewer identity,
// so it cannot rewrite a config file the interactive xeet also owns.
type Store interface {
	Version() (int, error)
	Accounts() ([]config.AccountInfo, error)
	LoadAccount(userID string) (*config.Config, error)
}

// account is one allowlisted account resolved against the saved roster.
type account struct {
	UserID string
	Handle string
	Info   config.AccountInfo
}

func normalizeSelector(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "@"))
}

func selectorMatches(selector string, userID, handle string) bool {
	selector = normalizeSelector(selector)
	if selector == "" {
		return false
	}
	return selector == userID || (handle != "" && selector == strings.ToLower(handle))
}

// resolveAccount turns the account_id argument into a saved account, allowing
// only what the deployment allowlisted. An omitted argument is accepted only
// when the allowlist names exactly one account; with several, guessing would
// read another account's bookmarks under a name nobody typed.
func (s *Server) resolveAccount(requested string) (account, *Failure) {
	if len(s.allowed) == 0 {
		return account{}, failf(CodeNotConfigured, "this server allows no accounts; start it with --allow-account <handle|user-id>")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if len(s.allowed) != 1 {
			return account{}, failf(CodeAccountRequired,
				fmt.Sprintf("account_id is required because several accounts are allowed: %s", strings.Join(s.allowed, ", ")))
		}
		requested = s.allowed[0]
	}
	if len(requested) > maxAccountIDLength {
		return account{}, failf(CodeInvalidArgument, "account_id is too long")
	}

	accounts, err := s.store.Accounts()
	if err != nil {
		return account{}, failf(CodeInternal, "read saved accounts: "+err.Error())
	}

	// Match the request against the allowlist first, then the allowlisted
	// entry against the roster, so a saved-but-not-allowed account reads as a
	// mismatch rather than leaking that it exists.
	var allowedEntry string
	for _, entry := range s.allowed {
		if normalizeSelector(entry) == normalizeSelector(requested) {
			allowedEntry = entry
			break
		}
		for _, saved := range accounts {
			if selectorMatches(entry, saved.UserID, saved.Handle) && selectorMatches(requested, saved.UserID, saved.Handle) {
				allowedEntry = entry
				break
			}
		}
		if allowedEntry != "" {
			break
		}
	}
	if allowedEntry == "" {
		return account{}, failf(CodeAccountMismatch, "account_id is not one this server is allowed to read")
	}

	for _, saved := range accounts {
		if selectorMatches(allowedEntry, saved.UserID, saved.Handle) {
			return account{UserID: saved.UserID, Handle: saved.Handle, Info: saved}, nil
		}
	}
	version, versionErr := s.store.Version()
	if versionErr == nil && version == 0 {
		return account{}, failf(CodeAuthRequired,
			"no saved accounts in the multi-account layout; run 'xeet auth' (or 'xeet accounts' once to migrate an older session)")
	}
	return account{}, failf(CodeAuthRequired,
		fmt.Sprintf("account %s is allowed but has no saved session; run 'xeet auth' for it", allowedEntry))
}

// sessionCache keeps one web client per account for the life of the process.
// The client caches X's transaction-id key material for fifteen minutes, so
// rebuilding it per call would cost an extra page load each time. The entry is
// keyed by a fingerprint of the cookie pair, so a re-authenticated keyring
// entry is picked up on the next call without a restart.
type sessionCache struct {
	mu       sync.Mutex
	sessions map[string]cachedSession
}

type cachedSession struct {
	fingerprint string
	session     Session
}

func sessionFingerprint(cfg *config.Config) string {
	sum := sha256.Sum256([]byte(cfg.AuthToken + "\x00" + cfg.CT0))
	return hex.EncodeToString(sum[:8])
}

func (s *Server) openSession(acct account) (Session, *config.Config, *Failure) {
	cfg, err := s.store.LoadAccount(acct.UserID)
	if err != nil {
		if errors.Is(err, config.ErrSessionIncomplete) {
			return nil, nil, failf(CodeAuthRequired, err.Error())
		}
		return nil, nil, failf(CodeKeyringUnavailable, "read the session from the OS keyring: "+err.Error())
	}
	if cfg.AuthToken == "" || cfg.CT0 == "" {
		return nil, nil, failf(CodeAuthRequired, "saved account has no session; run 'xeet auth'")
	}

	fingerprint := sessionFingerprint(cfg)
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	if cached, ok := s.cache.sessions[acct.UserID]; ok && cached.fingerprint == fingerprint {
		return cached.session, cfg, nil
	}
	session := s.newSession(cfg)
	s.cache.sessions[acct.UserID] = cachedSession{fingerprint: fingerprint, session: session}
	return session, cfg, nil
}
