package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/melqtx/xeet/pkg/config"
)

func stubDiscoverQIDs(t *testing.T, found map[string]string) *[]string {
	t.Helper()
	var asked []string
	orig := discoverQIDsViaCDP
	discoverQIDsViaCDP = func(_ context.Context, endpoint string, ops []string) (map[string]string, error) {
		if endpoint != "http://127.0.0.1:9222" {
			t.Errorf("endpoint = %q", endpoint)
		}
		asked = ops
		return found, nil
	}
	t.Cleanup(func() { discoverQIDsViaCDP = orig })
	return &asked
}

func TestDiscoverQidsCommandSavesFoundQIDs(t *testing.T) {
	dir := t.TempDir()
	existing := "version: 2\nsearch_timeline_qid: keepThisSearchQID\n"
	if err := os.WriteFile(filepath.Join(dir, ".xeet.yaml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	asked := stubDiscoverQIDs(t, map[string]string{"ListLatestTweetsTimeline": "listLatestQID_abcdefghij"})
	mgr := config.NewConfigManagerAt(dir, nil)

	var out bytes.Buffer
	if err := runDiscoverQIDs(context.Background(), &out, mgr, "http://127.0.0.1:9222"); err != nil {
		t.Fatal(err)
	}

	if strings.Join(*asked, ",") != "ListLatestTweetsTimeline,ListsManagementPageTimeline" {
		t.Fatalf("asked the browser for %v", *asked)
	}
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListLatestTweetsTimelineQID != "listLatestQID_abcdefghij" {
		t.Fatalf("ListLatestTweetsTimeline qid = %q", cfg.ListLatestTweetsTimelineQID)
	}
	if cfg.ListsManagementPageTimelineQID != "" {
		t.Fatalf("an operation that was not found got a qid: %q", cfg.ListsManagementPageTimelineQID)
	}
	if cfg.SearchTimelineQID != "keepThisSearchQID" {
		t.Fatalf("an unrelated cached qid was lost: %q", cfg.SearchTimelineQID)
	}
	want := "ListLatestTweetsTimeline     saved (len=24)\nListsManagementPageTimeline  not found\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	if strings.Contains(out.String(), "listLatestQID") {
		t.Fatal("the qid itself must not be printed, only its length")
	}
}

func TestDiscoverQidsCommandExitsNonZeroWhenNothingFound(t *testing.T) {
	dir := t.TempDir()
	stubDiscoverQIDs(t, map[string]string{})
	mgr := config.NewConfigManagerAt(dir, nil)

	var out bytes.Buffer
	err := runDiscoverQIDs(context.Background(), &out, mgr, "http://127.0.0.1:9222")
	if !errors.Is(err, errNoQIDsDiscovered) || ExitCode(err) != 1 {
		t.Fatalf("err = %v (exit %d), want errNoQIDsDiscovered with exit 1", err, ExitCode(err))
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".xeet.yaml")); !os.IsNotExist(statErr) {
		t.Fatal("a run that found nothing must not write the config")
	}
	if strings.Count(out.String(), "not found") != 2 {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDiscoverQidsCommandRequiresEndpoint(t *testing.T) {
	flag := discoverQIDsCmd.Flags().Lookup("from-cdp")
	if flag == nil || flag.Annotations["cobra_annotation_bash_completion_one_required_flag"] == nil {
		t.Fatal("--from-cdp must be a required flag")
	}
	found := false
	for _, c := range rootCmd.Commands() {
		found = found || c.Name() == "discover-qids"
	}
	if !found {
		t.Fatal("discover-qids is not registered on the root command")
	}
}
