package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

func TestLoginResultFromCDPCookiesPairsWithinOneDomain(t *testing.T) {
	cookies := []cdpCookie{
		{Name: "auth_token", Value: "tw-auth", Domain: ".twitter.com"},
		{Name: "ct0", Value: "x-ct0", Domain: ".x.com"},
		{Name: "auth_token", Value: "x-auth", Domain: ".x.com", Expires: 1900000000},
		{Name: "other", Value: "ignored", Domain: ".example.com"},
	}
	got, err := loginResultFromCDPCookies(cookies)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthToken != "x-auth" || got.CT0 != "x-ct0" || got.CookieDomain != ".x.com" {
		t.Fatalf("mixed domains: %+v", got)
	}
	if got.ExpiresAt.Unix() != 1900000000 {
		t.Fatalf("expiry not carried: %v", got.ExpiresAt)
	}
}

func TestLoginResultFromCDPCookiesRejectsHalfPair(t *testing.T) {
	_, err := loginResultFromCDPCookies([]cdpCookie{{Name: "auth_token", Value: "a", Domain: ".x.com"}})
	if err == nil {
		t.Fatal("auth_token without ct0 must not produce a session")
	}
}

func TestLoginResultFromCDPCookiesIgnoresForeignDomains(t *testing.T) {
	_, err := loginResultFromCDPCookies([]cdpCookie{
		{Name: "auth_token", Value: "a", Domain: ".notx.com"},
		{Name: "ct0", Value: "c", Domain: ".notx.com"},
	})
	if err == nil {
		t.Fatal("lookalike domain accepted")
	}
}

// fakeCDPBrowser serves /json/version and a browser WebSocket that answers the
// Target and Runtime commands DiscoverQueryIDsViaCDP issues. evaluated records
// the Runtime.evaluate request so tests can check where it was routed.
type fakeCDPBrowser struct {
	pages     []map[string]string
	bundle    map[string]string
	mu        sync.Mutex
	evaluated map[string]any
}

func (f *fakeCDPBrowser) lastEvaluate() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evaluated
}

func (f *fakeCDPBrowser) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	upgrader := websocket.Upgrader{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"webSocketDebuggerUrl": "ws://127.0.0.1:1/devtools/browser/fake",
			})
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			// An unsolicited event first, as a real browser sends them freely.
			_ = conn.WriteJSON(map[string]any{"method": "Target.targetInfoChanged"})
			var result any
			switch msg["method"] {
			case "Target.getTargets":
				infos := []map[string]string{}
				for i, p := range f.pages {
					infos = append(infos, map[string]string{"targetId": fmt.Sprint("T", i), "type": p["type"], "url": p["url"]})
				}
				result = map[string]any{"targetInfos": infos}
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "S1"}
			case "Runtime.evaluate":
				f.mu.Lock()
				f.evaluated = msg
				f.mu.Unlock()
				result = map[string]any{"result": map[string]any{"type": "object", "value": f.bundle}}
			default:
				result = map[string]any{}
			}
			_ = conn.WriteJSON(map[string]any{"id": msg["id"], "result": result})
		}
	}))
	return srv
}

func TestDiscoverQueryIDsViaCDPFindsOperationInBundle(t *testing.T) {
	f := &fakeCDPBrowser{
		pages: []map[string]string{
			{"type": "service_worker", "url": "https://x.com/sw.js"},
			{"type": "page", "url": "https://notx.com/home"},
			{"type": "page", "url": "https://x.com/i/lists/1"},
		},
		bundle: map[string]string{
			"ListLatestTweetsTimeline": `e.exports={queryId:"neighbourQID99",operationName:"ListPinOne",operationType:"mutation"}},` +
				`5678:e=>{e.exports={queryId:"testQID123",operationName:"ListLatestTweetsTimeline",operationType:"query"}}`,
		},
	}
	srv := f.serve(t)
	defer srv.Close()

	got, err := DiscoverQueryIDsViaCDP(context.Background(), srv.URL,
		[]string{"ListLatestTweetsTimeline", "ListsManagementPageTimeline"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["ListLatestTweetsTimeline"] != "testQID123" {
		t.Fatalf("discovered %v, want only ListLatestTweetsTimeline=testQID123", got)
	}
	evaluated := f.lastEvaluate()
	if evaluated["sessionId"] != "S1" {
		t.Fatalf("Runtime.evaluate was not routed to the attached x.com page: %v", evaluated["sessionId"])
	}
	params, _ := evaluated["params"].(map[string]any)
	expr, _ := params["expression"].(string)
	if !strings.Contains(expr, `["ListLatestTweetsTimeline","ListsManagementPageTimeline"]`) || params["awaitPromise"] != true {
		t.Fatalf("script did not receive the operations or does not await: %v", params)
	}
}

func TestDiscoverQueryIDsViaCDPNoXcomPage(t *testing.T) {
	f := &fakeCDPBrowser{pages: []map[string]string{
		{"type": "page", "url": "https://example.com/?next=https://x.com"},
		{"type": "service_worker", "url": "https://x.com/sw.js"},
	}}
	srv := f.serve(t)
	defer srv.Close()

	_, err := DiscoverQueryIDsViaCDP(context.Background(), srv.URL, []string{"ListLatestTweetsTimeline"})
	if err == nil || !strings.Contains(err.Error(), "no active x.com tab found") {
		t.Fatalf("err = %v, want the no-x.com-tab error", err)
	}
	if f.lastEvaluate() != nil {
		t.Fatal("evaluated a script although no x.com page exists")
	}
}

func TestDiscoverQueryIDsViaCDPReturnsEmptyWhenNothingMatches(t *testing.T) {
	f := &fakeCDPBrowser{
		pages:  []map[string]string{{"type": "page", "url": "https://x.com/home"}},
		bundle: map[string]string{},
	}
	srv := f.serve(t)
	defer srv.Close()

	got, err := DiscoverQueryIDsViaCDP(context.Background(), srv.URL, []string{"ListLatestTweetsTimeline"})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an empty map and no error", got, err)
	}
}
