package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/melqtx/xeet/pkg/config"
	xmcp "github.com/melqtx/xeet/pkg/mcp"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

const (
	allowedAccountsEnv = "XEET_MCP_ALLOWED_ACCOUNTS"
	secretsFileEnv     = "XEET_MCP_SECRETS_FILE"
)

var (
	mcpAllowAccounts []string
	mcpTimeout       time.Duration
	mcpHealthTimeout time.Duration
	mcpMaxPages      int
	mcpMaxConcurrent int
	mcpPageDelay     time.Duration
	mcpSecretsFile   string
	mcpAllowWrite    bool
	mcpWriteQuota    int

	mcpCallQuery   string
	mcpCallAccount string
	mcpCallLimit   int
	mcpCallCursor  string
	mcpCallTimeout int
	mcpCallText    string
	mcpCallPostID  string
	mcpCallReplyTo string
	mcpCallUnlike  bool
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "serve read-only X search and bookmarks over the Model Context Protocol",
	Long: `xeet mcp exposes three read-only tools (search_x_posts, get_x_bookmarks,
get_x_session_health) to MCP clients such as ChatGPT, Claude, or an editor.

Only accounts named with --allow-account (or ` + allowedAccountsEnv + `) can be
reached; there is no fallback to the active account.

Writing is off unless --allow-write is passed, and then the surface is exactly
two tools: post_x_post (post or reply) and set_x_post_like. Reposting, quoting,
bookmarking, following, direct messages and media uploads are not implemented
here and cannot be reached from MCP.`,
	Example: `  xeet mcp serve --allow-account @alice                 # read-only stdio server
  xeet mcp serve --allow-account @alice --allow-write   # also posting and likes
  xeet mcp tools --allow-account @alice                 # what a client would see
  xeet mcp call get_x_session_health --allow-account @alice
  xeet mcp call search_x_posts --allow-account @alice --query "go tui" --limit 5
  xeet mcp call post_x_post --allow-account @alice --allow-write --text "hello"`,
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "run the MCP server on stdin/stdout",
	Args:  cobra.NoArgs,
	RunE:  runMCPServe,
}

var mcpToolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "print the tool list an MCP client would receive, as JSON",
	Args:  cobra.NoArgs,
	RunE:  runMCPTools,
}

var mcpCallCmd = &cobra.Command{
	Use:   "call <tool>",
	Short: "invoke one tool through an in-process MCP client and print its result",
	Long: `call runs the same initialize, tools/list, and tools/call exchange a
remote client would, over an in-memory transport, and prints the structured
result. Exit status: 0 for ok or empty, 2 for partial, 1 for error.`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: xmcp.ToolNames(true),
	RunE:      runMCPCall,
}

func init() {
	for _, command := range []*cobra.Command{mcpServeCmd, mcpToolsCmd, mcpCallCmd} {
		command.Flags().StringArrayVar(&mcpAllowAccounts, "allow-account", nil,
			"saved account (handle or user id) tools may read; repeatable. Falls back to "+allowedAccountsEnv+" (comma-separated)")
		command.Flags().DurationVar(&mcpTimeout, "timeout", xmcp.DefaultTimeout, "upper bound per search or bookmarks call (max 60s)")
		command.Flags().DurationVar(&mcpHealthTimeout, "health-timeout", xmcp.DefaultHealthTimeout, "upper bound for the live check in get_x_session_health (max 60s)")
		command.Flags().IntVar(&mcpMaxPages, "max-pages", xmcp.DefaultMaxPages, "upstream pages one call may fetch (max 10)")
		command.Flags().IntVar(&mcpMaxConcurrent, "max-concurrent", xmcp.DefaultMaxConcurrent, "tool calls allowed in flight at once")
		command.Flags().DurationVar(&mcpPageDelay, "page-delay", xmcp.DefaultPageDelay, "pause between upstream pages within one call")
		command.Flags().StringVar(&mcpSecretsFile, "secrets-file", "",
			"read session cookies from this 0600 file instead of the OS keyring (headless hosts). Falls back to "+secretsFileEnv)
		command.Flags().BoolVar(&mcpAllowWrite, "allow-write", false,
			"also expose post_x_post and set_x_post_like. Off by default: without it the server has no way to post or like")
		command.Flags().IntVar(&mcpWriteQuota, "write-quota", 0,
			"writes allowed per rolling hour across the process (default 10)")
	}
	mcpCallCmd.Flags().StringVar(&mcpCallQuery, "query", "", "search query (search_x_posts)")
	mcpCallCmd.Flags().StringVar(&mcpCallAccount, "account", "", "account_id argument (handle or user id)")
	mcpCallCmd.Flags().IntVar(&mcpCallLimit, "limit", 0, "limit argument, 1-100 (default 20)")
	mcpCallCmd.Flags().StringVar(&mcpCallCursor, "cursor", "", "cursor argument from a previous result")
	mcpCallCmd.Flags().IntVar(&mcpCallTimeout, "timeout-seconds", 0, "timeout_seconds argument, 1-60")
	mcpCallCmd.Flags().StringVar(&mcpCallText, "text", "", "text argument (post_x_post)")
	mcpCallCmd.Flags().StringVar(&mcpCallPostID, "post-id", "", "post_id argument (set_x_post_like)")
	mcpCallCmd.Flags().StringVar(&mcpCallReplyTo, "reply-to", "", "reply_to_id argument (post_x_post)")
	mcpCallCmd.Flags().BoolVar(&mcpCallUnlike, "unlike", false, "send liked=false (set_x_post_like)")

	mcpCmd.AddCommand(mcpServeCmd, mcpToolsCmd, mcpCallCmd)
	rootCmd.AddCommand(mcpCmd)
}

func mcpAllowedAccounts() []string {
	if len(mcpAllowAccounts) > 0 {
		return mcpAllowAccounts
	}
	var accounts []string
	for _, entry := range strings.Split(os.Getenv(allowedAccountsEnv), ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			accounts = append(accounts, entry)
		}
	}
	return accounts
}

// mcpStore picks the keyring. A headless host has no Secret Service, so the
// deployment can point at a private file; the OS user owning that file and
// the config becomes the boundary the keyring would otherwise be.
func mcpStore() (xmcp.Store, error) {
	path := mcpSecretsFile
	if path == "" {
		path = os.Getenv(secretsFileEnv)
	}
	if path == "" {
		return config.NewConfigManager()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return config.NewConfigManagerAt(home, config.NewFileSecretStore(path)), nil
}

func newMCPServer() (*xmcp.Server, error) {
	store, err := mcpStore()
	if err != nil {
		return nil, err
	}
	return xmcp.New(xmcp.Options{
		Store:           store,
		AllowedAccounts: mcpAllowedAccounts(),
		Timeout:         mcpTimeout,
		HealthTimeout:   mcpHealthTimeout,
		MaxPages:        mcpMaxPages,
		MaxConcurrent:   mcpMaxConcurrent,
		AllowWrite:      mcpAllowWrite,
		WriteQuota:      mcpWriteQuota,
		PageDelay:       mcpPageDelay,
		Stderr:          os.Stderr,
		Version:         appVersion,
	})
}

func runMCPServe(cmd *cobra.Command, args []string) error {
	server, err := newMCPServer()
	if err != nil {
		return err
	}
	// Stdout belongs to the protocol from here on; anything else the process
	// wants to say goes to stderr through the server's logger.
	return server.Run(cmd.Context())
}

// connectInProcess wires an MCP client to the server over memory pipes so the
// local commands exercise the protocol path, not the handlers directly.
func connectInProcess(ctx context.Context, server *xmcp.Server) (*sdk.ClientSession, func(), error) {
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	serverSession, err := server.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "xeet-cli", Version: appVersion}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	}
	return clientSession, cleanup, nil
}

func runMCPTools(cmd *cobra.Command, args []string) error {
	server, err := newMCPServer()
	if err != nil {
		return err
	}
	session, cleanup, err := connectInProcess(cmd.Context(), server)
	if err != nil {
		return err
	}
	defer cleanup()
	tools, err := session.ListTools(cmd.Context(), nil)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(tools)
}

// errPartial and errToolFailed carry the exit status a script can branch on;
// the JSON on stdout already says what happened.
var (
	errPartial    = errors.New("partial result")
	errToolFailed = errors.New("tool call failed")
)

func runMCPCall(cmd *cobra.Command, args []string) error {
	tool := args[0]
	arguments := map[string]any{}
	if mcpCallQuery != "" {
		arguments["query"] = mcpCallQuery
	}
	if mcpCallAccount != "" {
		arguments["account_id"] = mcpCallAccount
	}
	if mcpCallLimit != 0 {
		arguments["limit"] = mcpCallLimit
	}
	if mcpCallCursor != "" {
		arguments["cursor"] = mcpCallCursor
	}
	if mcpCallTimeout != 0 {
		arguments["timeout_seconds"] = mcpCallTimeout
	}
	if mcpCallText != "" {
		arguments["text"] = mcpCallText
	}
	if mcpCallPostID != "" {
		arguments["post_id"] = mcpCallPostID
	}
	if mcpCallReplyTo != "" {
		arguments["reply_to_id"] = mcpCallReplyTo
	}
	if mcpCallUnlike {
		arguments["liked"] = false
	}

	server, err := newMCPServer()
	if err != nil {
		return err
	}
	session, cleanup, err := connectInProcess(cmd.Context(), server)
	if err != nil {
		return err
	}
	defer cleanup()

	result, err := session.CallTool(cmd.Context(), &sdk.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		return fmt.Errorf("tools/call %s: %w", tool, err)
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result.StructuredContent); err != nil {
		return err
	}
	status, _ := result.StructuredContent.(map[string]any)["status"].(string)
	switch {
	case result.IsError || status == xmcp.StatusError:
		return errToolFailed
	case status == xmcp.StatusPartial:
		return errPartial
	}
	return nil
}

// ExitCode maps the errors Execute returns onto process exit statuses: 2 for a
// partial MCP tool result, 1 for everything else that failed.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errPartial):
		return 2
	}
	return 1
}
