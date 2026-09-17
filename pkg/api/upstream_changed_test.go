package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestMalformedTimelineBodyIsReportedAsAnUpstreamChange(t *testing.T) {
	for _, body := range []string{`{"nope":1}`, `[]`, `"text"`} {
		client := newTestClient(func(req *http.Request) (*http.Response, error) {
			return response(http.StatusOK, body), nil
		})
		_, err := client.FetchBookmarks(context.Background(), "", 5)
		if !errors.Is(err, ErrUpstreamChanged) {
			t.Fatalf("body %s: err = %v, want ErrUpstreamChanged", body, err)
		}
	}
}

func TestFailedQueryIDDiscoveryKeepsBothTheShapeAndTransportReason(t *testing.T) {
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, ""), nil
	})
	transport := &ConnectionError{Kind: "timeout", Err: context.DeadlineExceeded}
	client.discover = func(context.Context, string, string, string) (string, error) {
		return "", transport
	}
	_, err := client.FetchBookmarks(context.Background(), "", 5)
	if !errors.Is(err, ErrUpstreamChanged) {
		t.Fatalf("err = %v, want ErrUpstreamChanged", err)
	}
	var connection *ConnectionError
	if !errors.As(err, &connection) || connection.Kind != "timeout" {
		t.Fatalf("err = %v, want the transport reason preserved", err)
	}
}

func TestEngagementKnownDistinguishesMissingCountersFromZero(t *testing.T) {
	withCounters := map[string]any{
		"tweet_results": map[string]any{"result": map[string]any{
			"rest_id": "1",
			"legacy":  map[string]any{"full_text": "hi", "favorite_count": float64(0), "reply_count": float64(0), "retweet_count": float64(0)},
		}},
	}
	post, ok := parseTimelineItem(withCounters)
	if !ok || !post.EngagementKnown || post.LikeCount != 0 {
		t.Fatalf("with counters: %+v ok=%v", post, ok)
	}
	withoutCounters := map[string]any{
		"tweet_results": map[string]any{"result": map[string]any{
			"rest_id": "2",
			"legacy":  map[string]any{"full_text": "hi"},
		}},
	}
	post, ok = parseTimelineItem(withoutCounters)
	if !ok || post.EngagementKnown || post.LikeCount != 0 {
		t.Fatalf("without counters: %+v ok=%v", post, ok)
	}
}
