package api

import (
	"context"
	"os"
	"testing"
	"time"
)

// Gated live test: reads the query ids out of a real Chromium with an x.com
// tab open (a list page loads the list chunks). Run with XEET_LIVE_DISCOVER_CDP=1; the endpoint
// defaults to http://127.0.0.1:9222 and can be moved with XEET_LIVE_CDP.
func TestDiscoverQueryIDsViaCDPLive(t *testing.T) {
	if os.Getenv("XEET_LIVE_DISCOVER_CDP") != "1" {
		t.Skip("set XEET_LIVE_DISCOVER_CDP=1 to run the live CDP discovery test")
	}
	endpoint := os.Getenv("XEET_LIVE_CDP")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:9222"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()

	found, err := DiscoverQueryIDsViaCDP(ctx, endpoint, []string{"ListLatestTweetsTimeline", "ListsManagementPageTimeline"})
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	qid, ok := found["ListLatestTweetsTimeline"]
	if !ok {
		t.Fatalf("ListLatestTweetsTimeline not found in the x.com tab (found %d operations)", len(found))
	}
	if len(qid) < 8 {
		t.Fatalf("suspicious queryId length %d", len(qid))
	}
	t.Logf("found %d operations; ListLatestTweetsTimeline len=%d", len(found), len(qid))
}
