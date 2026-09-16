package mcp

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/melqtx/xeet/pkg/api"
	"github.com/melqtx/xeet/pkg/config"
)

// Code is the stable, machine-readable failure class a tool result carries.
// Clients branch on these, so they are part of the output contract: add new
// ones, never rename existing ones.
type Code string

const (
	CodeInvalidArgument     Code = "INVALID_ARGUMENT"
	CodeInvalidCursor       Code = "INVALID_CURSOR"
	CodeNotConfigured       Code = "NOT_CONFIGURED"
	CodeAccountRequired     Code = "ACCOUNT_REQUIRED"
	CodeAccountMismatch     Code = "ACCOUNT_MISMATCH"
	CodeAuthRequired        Code = "AUTH_REQUIRED"
	CodeKeyringUnavailable  Code = "KEYRING_UNAVAILABLE"
	CodeRateLimited         Code = "RATE_LIMITED"
	CodeTimeout             Code = "TIMEOUT"
	CodeUpstreamChanged     Code = "UPSTREAM_CHANGED"
	CodeUpstreamUnavailable Code = "UPSTREAM_UNAVAILABLE"
	CodeUpstreamError       Code = "UPSTREAM_ERROR"
	CodeNetworkError        Code = "NETWORK_ERROR"
	CodePartialResult       Code = "PARTIAL_RESULT"
	CodeBusy                Code = "BUSY"
	CodeInternal            Code = "INTERNAL"
)

// Failure is the error object embedded in a tool result. Cause is set only on
// PARTIAL_RESULT and names the class of the page fetch that stopped the call.
type Failure struct {
	Code              Code   `json:"code"`
	Message           string `json:"message"`
	Cause             Code   `json:"cause,omitempty"`
	RetryAfterSeconds *int   `json:"retry_after_seconds,omitempty"`
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	return string(f.Code) + ": " + f.Message
}

func failf(code Code, message string) *Failure {
	return &Failure{Code: code, Message: sanitize(message)}
}

// classify maps an error from pkg/api or pkg/config onto a Failure. The order
// matters: a discovery that timed out wraps both ErrUpstreamChanged and a
// timeout, and the transport reason is the more actionable of the two.
func classify(err error) *Failure {
	if err == nil {
		return nil
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}

	var rateLimit *api.RateLimitError
	if errors.As(err, &rateLimit) {
		f := failf(CodeRateLimited, err.Error())
		if !rateLimit.Reset.IsZero() {
			seconds := int(time.Until(rateLimit.Reset).Round(time.Second).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			f.RetryAfterSeconds = &seconds
		}
		return f
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failf(CodeTimeout, "x did not respond within the call timeout")
	}
	if errors.Is(err, context.Canceled) {
		return failf(CodeTimeout, "the call was cancelled before x responded")
	}
	var connection *api.ConnectionError
	if errors.As(err, &connection) {
		if connection.Kind == "timeout" {
			return failf(CodeTimeout, err.Error())
		}
		return failf(CodeNetworkError, err.Error())
	}
	if errors.Is(err, api.ErrSessionExpired) {
		return failf(CodeAuthRequired, err.Error())
	}
	if errors.Is(err, config.ErrSessionIncomplete) {
		return failf(CodeAuthRequired, err.Error())
	}
	if errors.Is(err, api.ErrUpstreamChanged) {
		return failf(CodeUpstreamChanged, err.Error())
	}
	var unavailable *api.ServiceUnavailableError
	if errors.As(err, &unavailable) {
		return failf(CodeUpstreamUnavailable, err.Error())
	}
	return failf(CodeUpstreamError, err.Error())
}

// sessionSecretPattern matches the two cookie values xeet holds. Neither
// pkg/api nor pkg/config puts them in error text, so this is belt and braces
// for the day an upstream library echoes a request header back.
var sessionSecretPattern = regexp.MustCompile(`(?i)(auth_token|ct0)(=|: ?)[^;,\s"]+`)

var whitespace = regexp.MustCompile(`\s+`)

const maxMessageLength = 300

func sanitize(message string) string {
	message = sessionSecretPattern.ReplaceAllString(message, "$1$2[redacted]")
	message = strings.TrimSpace(whitespace.ReplaceAllString(message, " "))
	if len(message) > maxMessageLength {
		message = message[:maxMessageLength] + "…"
	}
	return message
}
