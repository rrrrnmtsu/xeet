package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Write tool names. They are registered only when the server was started with
// writes enabled, so a read-only deployment does not merely refuse them: they
// are absent from tools/list.
const (
	ToolPostPost = "post_x_post"
	ToolSetLike  = "set_x_post_like"
)

const (
	maxPostRunes = 280
	// defaultWriteQuota bounds writes per rolling hour for the whole process.
	// The point is not politeness to X: it is that an MCP client driven by a
	// model reading untrusted post text should not be able to turn one bad
	// instruction into a stream of posts.
	defaultWriteQuota  = 10
	defaultWriteWindow = time.Hour
	defaultWriteTimout = 60 * time.Second
)

type PostInput struct {
	Text           string `json:"text" jsonschema:"The post text, 1-280 characters. Sent to X verbatim: no signature, hashtag, or link is added. Newlines are allowed."`
	AccountID      string `json:"account_id,omitempty" jsonschema:"Saved account to post as: a numeric X user id or @handle. Must be one the server allows. Optional only when the server allows exactly one account."`
	ReplyToID      string `json:"reply_to_id,omitempty" jsonschema:"Numeric id of the post to reply to. Omit for a standalone post."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Upper bound for the call, 1-60 seconds. Default 60; posting is slower than reading."`
}

type LikeInput struct {
	PostID         string `json:"post_id" jsonschema:"Numeric id of the post to like or unlike."`
	Liked          *bool  `json:"liked,omitempty" jsonschema:"true to like (default), false to remove an existing like."`
	AccountID      string `json:"account_id,omitempty" jsonschema:"Saved account to act as: a numeric X user id or @handle. Must be one the server allows."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Upper bound for the call, 1-60 seconds. Default 30."`
}

// WriteResult reports one mutation.
//
// applied=true means X confirmed it. applied=false with
// error.code=AMBIGUOUS_WRITE means the post may or may not exist and the
// caller must check the profile before retrying; every other error with
// applied=false means nothing was written.
type WriteResult struct {
	Status        string   `json:"status"`
	Tool          string   `json:"tool"`
	Action        string   `json:"action"`
	AccountID     string   `json:"account_id"`
	AccountHandle string   `json:"account_handle,omitempty"`
	Applied       bool     `json:"applied"`
	PostID        string   `json:"post_id,omitempty"`
	PostURL       string   `json:"post_url,omitempty"`
	ReplyToID     string   `json:"reply_to_id,omitempty"`
	Liked         *bool    `json:"liked,omitempty"`
	AttemptedAt   string   `json:"attempted_at"`
	WritesLeft    int      `json:"writes_left_this_hour"`
	Note          string   `json:"note,omitempty"`
	Error         *Failure `json:"error,omitempty"`
}

// writeQuota is a rolling-window counter shared by every write tool.
type writeQuota struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events []time.Time
}

func (q *writeQuota) prune(now time.Time) {
	cutoff := now.Add(-q.window)
	kept := q.events[:0]
	for _, at := range q.events {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	q.events = kept
}

// reserve takes a slot, returning how many remain. A write that turns out to
// fail before reaching X releases its slot again through release.
func (q *writeQuota) reserve(now time.Time) (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(now)
	if len(q.events) >= q.limit {
		return 0, false
	}
	q.events = append(q.events, now)
	return q.limit - len(q.events), true
}

func (q *writeQuota) release(at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := len(q.events) - 1; i >= 0; i-- {
		if q.events[i].Equal(at) {
			q.events = append(q.events[:i], q.events[i+1:]...)
			return
		}
	}
}

func (q *writeQuota) remaining(now time.Time) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.prune(now)
	return q.limit - len(q.events)
}

// Invisible runes X renders as nothing. A post whose text differs from what
// the caller reviewed is the failure mode worth refusing outright.
const (
	zeroWidthSpace = '\u200b'
	byteOrderMark  = '\ufeff'
)

var postIDPattern = regexp.MustCompile(`^[0-9]{1,32}$`)

func validatePostID(field, value string) *Failure {
	value = strings.TrimSpace(value)
	if value == "" {
		return failf(CodeInvalidArgument, field+" must not be empty")
	}
	if !postIDPattern.MatchString(value) {
		return failf(CodeInvalidArgument, field+" must be a numeric X post id")
	}
	return nil
}

// validatePostText rejects what X would reject anyway, plus the invisible
// control characters that make a post's rendered text differ from the text the
// caller reviewed.
func validatePostText(text string) *Failure {
	if strings.TrimSpace(text) == "" {
		return failf(CodeInvalidArgument, "text must not be empty")
	}
	if !utf8.ValidString(text) {
		return failf(CodeInvalidArgument, "text is not valid UTF-8")
	}
	if count := utf8.RuneCountInString(text); count > maxPostRunes {
		return failf(CodeInvalidArgument, fmt.Sprintf("text is %d characters; the limit is %d", count, maxPostRunes))
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || r == zeroWidthSpace || r == byteOrderMark {
			return failf(CodeInvalidArgument, "text contains control or zero-width characters")
		}
	}
	return nil
}

// writeSessionFor resolves the account and returns a session that can mutate.
// The type assertion is the enforcement point: with writes disabled the server
// never registers these tools, and a session that cannot write cannot be
// coerced into writing by any argument.
func (s *Server) writeSessionFor(requested string) (WriteSession, account, *Failure) {
	if !s.allowWrite {
		return nil, account{}, failf(CodeWriteDisabled, "this server is read-only; start it with --allow-write to enable posting and likes")
	}
	acct, failure := s.resolveAccount(requested)
	if failure != nil {
		return nil, account{}, failure
	}
	session, _, failure := s.openSession(acct)
	if failure != nil {
		return nil, acct, failure
	}
	writer, ok := session.(WriteSession)
	if !ok {
		return nil, acct, failf(CodeWriteDisabled, "this session cannot write")
	}
	return writer, acct, nil
}

func (s *Server) runPost(ctx context.Context, in PostInput) WriteResult {
	now := s.now()
	result := WriteResult{
		Tool:        ToolPostPost,
		Action:      "create_post",
		ReplyToID:   strings.TrimSpace(in.ReplyToID),
		AttemptedAt: now.UTC().Format(time.RFC3339),
		WritesLeft:  s.writes.remaining(now),
	}
	fail := func(f *Failure) WriteResult {
		result.Status = StatusError
		result.Error = f
		return result
	}

	if f := validatePostText(in.Text); f != nil {
		return fail(f)
	}
	if result.ReplyToID != "" {
		if f := validatePostID("reply_to_id", result.ReplyToID); f != nil {
			return fail(f)
		}
	}
	timeout, f := s.callTimeout(in.TimeoutSeconds, defaultWriteTimout)
	if f != nil {
		return fail(f)
	}
	session, acct, f := s.writeSessionFor(in.AccountID)
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

	reservedAt := s.now()
	left, ok := s.writes.reserve(reservedAt)
	if !ok {
		return fail(failf(CodeWriteQuota, fmt.Sprintf("this server allows %d writes per hour and the window is full", s.writes.limit)))
	}
	result.WritesLeft = left

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No media: attaching files would mean the server reads paths a model
	// chose, which is a different trust boundary than text.
	postID, err := session.PostTweet(ctx, in.Text, result.ReplyToID, nil, nil)
	if err != nil {
		failure := classify(err)
		// An ambiguous post may exist, so its slot stays spent; everything
		// else definitely did not post and gives the slot back.
		if failure.Code != CodeAmbiguousWrite {
			s.writes.release(reservedAt)
			result.WritesLeft = s.writes.remaining(s.now())
		}
		if failure.Code == CodeAmbiguousWrite {
			result.Note = "do not retry automatically; check the profile first"
		}
		return fail(failure)
	}

	result.Status = StatusOK
	result.Applied = true
	result.PostID = postID
	if acct.Handle != "" && postID != "" {
		result.PostURL = "https://x.com/" + acct.Handle + "/status/" + postID
	}
	return result
}

func (s *Server) runLike(ctx context.Context, in LikeInput) WriteResult {
	now := s.now()
	liked := true
	if in.Liked != nil {
		liked = *in.Liked
	}
	action := "like_post"
	if !liked {
		action = "unlike_post"
	}
	result := WriteResult{
		Tool:        ToolSetLike,
		Action:      action,
		PostID:      strings.TrimSpace(in.PostID),
		Liked:       &liked,
		AttemptedAt: now.UTC().Format(time.RFC3339),
		WritesLeft:  s.writes.remaining(now),
	}
	fail := func(f *Failure) WriteResult {
		result.Status = StatusError
		result.Error = f
		return result
	}

	if f := validatePostID("post_id", result.PostID); f != nil {
		return fail(f)
	}
	timeout, f := s.callTimeout(in.TimeoutSeconds, s.timeout)
	if f != nil {
		return fail(f)
	}
	session, acct, f := s.writeSessionFor(in.AccountID)
	if f != nil {
		return fail(f)
	}
	result.AccountID = acct.UserID
	result.AccountHandle = acct.Handle
	result.PostURL = "https://x.com/i/status/" + result.PostID

	release, f := s.acquire(ctx)
	if f != nil {
		return fail(f)
	}
	defer release()

	reservedAt := s.now()
	left, ok := s.writes.reserve(reservedAt)
	if !ok {
		return fail(failf(CodeWriteQuota, fmt.Sprintf("this server allows %d writes per hour and the window is full", s.writes.limit)))
	}
	result.WritesLeft = left

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Liking is idempotent, so a failure here never leaves a half-applied
	// state and the quota slot goes back.
	if err := session.SetTweetLiked(ctx, result.PostID, liked); err != nil {
		s.writes.release(reservedAt)
		result.WritesLeft = s.writes.remaining(s.now())
		return fail(classify(err))
	}

	result.Status = StatusOK
	result.Applied = true
	return result
}
