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

// bundleScanJS looks, for every requested operation, for its descriptor in the
// page's JS and returns the text surrounding it. X moved list operations into
// lazily loaded chunks, so the static bundles DiscoverOperationQueryID reads no
// longer carry them.
//
// The chunks the page has loaded are searched first, through webpack's chunk
// registry: on a live x.com tab (2026-10) the list descriptors were there but
// in none of the 805 bundles the Service Worker had cached, which is searched
// only for what the registry lacks.
//
// The script returns surrounding text rather than the queryId itself so the
// choice of which nearby queryId belongs to the operation stays in
// findOperationQID, the same Go code the static discovery is tested against.
const bundleScanJS = `(async () => {
  const ops = %s;
  const radius = %d;
  const found = {};
  const take = (text) => {
    for (const op of ops) {
      if (found[op]) continue;
      const i = text.indexOf('operationName:"' + op + '"');
      if (i >= 0) found[op] = text.slice(Math.max(0, i - radius), i + radius);
    }
    return Object.keys(found).length === ops.length;
  };
  for (const key of Object.keys(window)) {
    if (!key.startsWith('webpackChunk') || !Array.isArray(window[key])) continue;
    for (const chunk of window[key]) {
      const modules = (chunk && chunk[1]) || {};
      for (const id in modules) {
        if (take(String(modules[id]))) return found;
      }
    }
  }
  for (const name of await caches.keys()) {
    if (!name.startsWith('assets_')) continue;
    const cache = await caches.open(name);
    for (const req of await cache.keys()) {
      if (!req.url.endsWith('.js')) continue;
      const resp = await cache.match(req);
      if (resp && take(await resp.text())) return found;
    }
  }
  return found;
})()`

const bundleScanRadius = 600

// DiscoverQueryIDsViaCDP scans the JS loaded by, and cached for, the first
// x.com page tab of a Chromium reachable at endpoint and returns
// operationName → queryId for the operations it found. Operations it could
// not find are absent from the map; finding none is not an error.
func DiscoverQueryIDsViaCDP(ctx context.Context, endpoint string, operations []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	wsURL, err := cdpBrowserWebSocket(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("connecting to the browser debugging socket: %w", err)
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(d)
	}
	c := &cdpConn{conn: conn}

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := c.call("", "Target.getTargets", nil, &targets); err != nil {
		return nil, err
	}
	targetID := ""
	for _, t := range targets.TargetInfos {
		if t.Type == "page" && isXPageURL(t.URL) {
			targetID = t.TargetID
			break
		}
	}
	if targetID == "" {
		return nil, fmt.Errorf("no active x.com tab found; open x.com in the browser first")
	}

	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call("", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	defer func() {
		_ = c.call("", "Target.detachFromTarget", map[string]any{"sessionId": attached.SessionID}, nil)
	}()

	opsJSON, err := json.Marshal(operations)
	if err != nil {
		return nil, err
	}
	var evaluated struct {
		Result struct {
			Value map[string]string `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{
		"expression":    fmt.Sprintf(bundleScanJS, opsJSON, bundleScanRadius),
		"awaitPromise":  true,
		"returnByValue": true,
	}
	if err := c.call(attached.SessionID, "Runtime.evaluate", params, &evaluated); err != nil {
		return nil, err
	}
	if ex := evaluated.ExceptionDetails; ex != nil {
		msg := ex.Text
		if ex.Exception != nil && ex.Exception.Description != "" {
			msg = ex.Exception.Description
		}
		return nil, fmt.Errorf("scanning the x.com tab for query ids: %s", msg)
	}

	found := make(map[string]string, len(evaluated.Result.Value))
	for _, op := range operations {
		if qid := findOperationQID(evaluated.Result.Value[op], op); qid != "" {
			found[op] = qid
		}
	}
	return found, nil
}

// isXPageURL matches the x.com host only, so a tab on a lookalike domain or a
// page that merely links to x.com is never evaluated in.
func isXPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "x.com" || strings.HasSuffix(host, ".x.com"))
}

// cdpConn issues CDP commands over one browser connection, routing commands to
// attached pages through flattened sessionIds.
type cdpConn struct {
	conn   *websocket.Conn
	nextID int
}

func (c *cdpConn) call(sessionID, method string, params any, out any) error {
	c.nextID++
	id := c.nextID
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	if err := c.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("sending %s to the browser: %w", method, err)
	}
	for {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := c.conn.ReadJSON(&reply); err != nil {
			return fmt.Errorf("reading the %s reply: %w", method, err)
		}
		// Events carry no id; replies to other commands are not ours.
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("browser refused %s: %s", method, reply.Error.Message)
		}
		if out == nil || len(reply.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(reply.Result, out); err != nil {
			return fmt.Errorf("decoding the %s reply: %w", method, err)
		}
		return nil
	}
}
