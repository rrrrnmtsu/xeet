package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/melqtx/xeet/pkg/api"
	"github.com/melqtx/xeet/pkg/config"

	"github.com/spf13/cobra"
)

var discoverQIDsFromCDP string

// discoverQIDsViaCDP is swapped out by tests so they need no browser.
var discoverQIDsViaCDP = api.DiscoverQueryIDsViaCDP

// cdpDiscoverableOperations are the operations X now ships only in lazily
// loaded chunks, out of reach of the static bundle discovery.
var cdpDiscoverableOperations = []struct {
	name string
	set  func(*config.Config, string)
}{
	{"ListLatestTweetsTimeline", func(c *config.Config, q string) { c.ListLatestTweetsTimelineQID = q }},
	{"ListsManagementPageTimeline", func(c *config.Config, q string) { c.ListsManagementPageTimelineQID = q }},
}

var errNoQIDsDiscovered = errors.New("none of the operations were found in the x.com tab; open a list on x.com and retry")

var discoverQIDsCmd = &cobra.Command{
	Use:   "discover-qids",
	Short: "find GraphQL query ids in a running browser's x.com tab",
	Long: `X loads some operations (list timelines) from chunks that only a running
x.com page pulls in, so xeet's own discovery cannot see them. This reads them
out of a Chromium started with --remote-debugging-port that has an x.com tab
open (a list page is the surest), and saves what it finds to ~/.xeet.yaml.`,
	Example: `  xeet discover-qids --from-cdp http://127.0.0.1:9222`,
	Args:    cobra.NoArgs,
	Hidden:  true, // advanced escape hatch, like setqid
	RunE: func(cmd *cobra.Command, args []string) error {
		configMgr, err := config.NewConfigManager()
		if err != nil {
			return err
		}
		return runDiscoverQIDs(cmd.Context(), cmd.OutOrStdout(), configMgr, discoverQIDsFromCDP)
	},
}

func init() {
	discoverQIDsCmd.Flags().StringVar(&discoverQIDsFromCDP, "from-cdp", "", "the browser's debugging endpoint (e.g. http://127.0.0.1:9222)")
	_ = discoverQIDsCmd.MarkFlagRequired("from-cdp")
	rootCmd.AddCommand(discoverQIDsCmd)
}

func runDiscoverQIDs(ctx context.Context, out io.Writer, configMgr *config.ConfigManager, endpoint string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	names := make([]string, len(cdpDiscoverableOperations))
	for i, op := range cdpDiscoverableOperations {
		names[i] = op.name
	}
	found, err := discoverQIDsViaCDP(ctx, endpoint, names)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		for _, name := range names {
			fmt.Fprintf(out, "%-28s not found\n", name)
		}
		return errNoQIDsDiscovered
	}

	cfg, err := configMgr.Load()
	if err != nil {
		return err
	}
	for _, op := range cdpDiscoverableOperations {
		if qid, ok := found[op.name]; ok {
			op.set(cfg, qid)
		}
	}
	// SaveQueryIDs rather than Save: it touches only the operation ids, so no
	// account or keyring entry is rewritten by a discovery run.
	if err := configMgr.SaveQueryIDs(cfg); err != nil {
		return err
	}
	for _, op := range cdpDiscoverableOperations {
		if qid, ok := found[op.name]; ok {
			fmt.Fprintf(out, "%-28s saved (len=%d)\n", op.name, len(qid))
		} else {
			fmt.Fprintf(out, "%-28s not found\n", op.name)
		}
	}
	return nil
}
