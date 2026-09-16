package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	cursorVersion   = 1
	maxCursorLength = 4096
)

// cursorToken binds X's opaque pagination cursor to the call that produced
// it. Reusing a bookmarks cursor on a search, or one account's cursor on
// another, would silently return the wrong page, so the token carries enough
// to refuse that. It deliberately holds no session material.
type cursorToken struct {
	Version   int    `json:"v"`
	Tool      string `json:"t"`
	AccountID string `json:"a"`
	QueryHash string `json:"q,omitempty"`
	Upstream  string `json:"c"`
}

func hashQuery(query string) string {
	if query == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(query))
	return hex.EncodeToString(sum[:8])
}

func encodeCursor(tool, accountID, query, upstream string) string {
	if upstream == "" {
		return ""
	}
	raw, _ := json.Marshal(cursorToken{
		Version:   cursorVersion,
		Tool:      tool,
		AccountID: accountID,
		QueryHash: hashQuery(query),
		Upstream:  upstream,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor returns the upstream cursor inside token when it was minted
// for exactly this tool, account, and query.
func decodeCursor(token, tool, accountID, query string) (string, *Failure) {
	if token == "" {
		return "", nil
	}
	if len(token) > maxCursorLength {
		return "", failf(CodeInvalidCursor, "cursor is too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", failf(CodeInvalidCursor, "cursor is not one this server issued")
	}
	var parsed cursorToken
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Version != cursorVersion || parsed.Upstream == "" {
		return "", failf(CodeInvalidCursor, "cursor is not one this server issued")
	}
	if parsed.Tool != tool {
		return "", failf(CodeInvalidCursor, fmt.Sprintf("cursor was issued by %s, not %s", parsed.Tool, tool))
	}
	if parsed.AccountID != accountID {
		return "", failf(CodeInvalidCursor, "cursor was issued for a different account")
	}
	if parsed.QueryHash != hashQuery(query) {
		return "", failf(CodeInvalidCursor, "cursor was issued for a different query")
	}
	return parsed.Upstream, nil
}
