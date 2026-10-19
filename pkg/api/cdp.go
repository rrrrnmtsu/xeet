package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ImportCDPSession reads the x.com session out of a Chromium that was started
// with --remote-debugging-port. It exists for hosts where the browser's cookie
// database cannot be decrypted from the xeet process (WSL reading a Windows
// Chrome, a container, a remote machine): the running browser decrypts its own
// cookies and hands them over the debugging protocol.
//
// endpoint is the http base ("http://127.0.0.1:9222"). Only auth_token and ct0
// are taken; no other cookie leaves the browser.
func ImportCDPSession(ctx context.Context, endpoint string) (*LoginResult, error) {
	wsURL, err := cdpBrowserWebSocket(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	// Chrome rejects handshakes carrying an unexpected Origin, so send none.
	conn, _, err := dialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("connecting to the browser debugging socket: %w", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Storage.getCookies"}); err != nil {
		return nil, fmt.Errorf("asking the browser for cookies: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	var reply struct {
		ID     int `json:"id"`
		Result struct {
			Cookies []cdpCookie `json:"cookies"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	for {
		reply.ID = 0
		if err := conn.ReadJSON(&reply); err != nil {
			return nil, fmt.Errorf("reading cookies from the browser: %w", err)
		}
		if reply.ID == 1 {
			break
		}
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("browser refused Storage.getCookies: %s", reply.Error.Message)
	}
	return loginResultFromCDPCookies(reply.Result.Cookies)
}

type cdpCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Expires float64 `json:"expires"`
}

func isXDomain(domain string) bool {
	d := strings.TrimPrefix(strings.ToLower(domain), ".")
	return d == "x.com" || strings.HasSuffix(d, ".x.com") ||
		d == "twitter.com" || strings.HasSuffix(d, ".twitter.com")
}

// loginResultFromCDPCookies picks one auth_token/ct0 pair from a single domain.
// Pairing by domain matters: x.com and twitter.com can both hold cookies, and a
// token from one with a ct0 from the other is rejected by X.
func loginResultFromCDPCookies(cookies []cdpCookie) (*LoginResult, error) {
	type pair struct {
		auth, ct0  cdpCookie
		hasA, hasC bool
	}
	byDomain := map[string]*pair{}
	for _, c := range cookies {
		if !isXDomain(c.Domain) || c.Value == "" {
			continue
		}
		p := byDomain[c.Domain]
		if p == nil {
			p = &pair{}
			byDomain[c.Domain] = p
		}
		switch c.Name {
		case "auth_token":
			p.auth, p.hasA = c, true
		case "ct0":
			p.ct0, p.hasC = c, true
		}
	}
	// Prefer the x.com host over legacy twitter.com.
	for _, domain := range []string{".x.com", "x.com", ".twitter.com", "twitter.com"} {
		p := byDomain[domain]
		if p == nil || !p.hasA || !p.hasC {
			continue
		}
		res := &LoginResult{
			AuthToken:    p.auth.Value,
			CT0:          p.ct0.Value,
			CookieDomain: domain,
		}
		if p.auth.Expires > 0 {
			res.ExpiresAt = time.Unix(int64(p.auth.Expires), 0)
		}
		return res, nil
	}
	return nil, fmt.Errorf("no signed-in x.com session in that browser (open x.com there and log in first)")
}

func cdpBrowserWebSocket(ctx context.Context, endpoint string) (string, error) {
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || base.Host == "" {
		return "", fmt.Errorf("invalid CDP endpoint %q (want http://127.0.0.1:9222)", endpoint)
	}
	if base.Scheme == "ws" || base.Scheme == "wss" {
		return base.String(), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching the browser at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("browser at %s answered HTTP %d", endpoint, resp.StatusCode)
	}
	var info struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("browser at %s did not report a debugging socket", endpoint)
	}
	// The browser reports its own host (often 127.0.0.1); keep the host the
	// caller reached it by so port-forwarded setups still connect.
	ws, err := url.Parse(info.WebSocketDebuggerURL)
	if err != nil {
		return "", err
	}
	ws.Host = base.Host
	return ws.String(), nil
}
