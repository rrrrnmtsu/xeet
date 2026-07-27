# SPEC: Bookmarks feed + Search (read-only) for xeet

Status: implementation-ready. Target executor: Codex (no further clarification available — every decision is made below; where reality may diverge, §8 tells you exactly what to do instead of guessing).

---

## 🇯🇵 なんJまとめ（読む人向け）

ワイらの xeet フォークに読み取り専用の機能を2つ足すで:

1. **ブックマーク feed** — 自分の X ブックマークを TUI で眺める（`b` キー / `--bookmarks` フラグ）
2. **検索** — `/` キーでクエリ入力 → 検索結果を feed として閲覧（`xeet search "query"` も可）

フェーズ構成（1 phase = 1 commit が基本）:

- **Phase 0**: `following bool` を feed-kind enum にリファクタ（挙動不変、upstream に PR 出せる粒度）
- **Phase 1**: Bookmarks API（`pkg/api/bookmarks.go`）
- **Phase 2**: Bookmarks の TUI キー + CLI フラグ
- **Phase 3**: Search API（`pkg/api/search.go`）+ entry-walker 共通化 + `operationHint` バグ修正
- **Phase 4**: 検索プロンプト mode（reply composer を雛形にした textinput）
- **Phase 5**: `cmd/search.go` サブコマンド
- **Phase 6**: テスト（mocked fixture + live test 2本、`go test -race` 通す）

規模感: 7 commits、実装 +600〜800行 / テスト +500〜700行くらいや。GraphQL の変数名だけは実セッションで裏取り必要（§8）。QID は絶対に捏造したらアカン、discover に任せるんや。

---

## 0. Ground rules (apply to every phase)

- Go 1.26.5 pinned — do not touch the `go` directive. No new dependencies (`bubbles/textinput` is already a direct dep via `go.mod`; `default.nix` `vendorHash` therefore stays unchanged — if you add ANY dep anyway, you must update `vendorHash` or the `nix` CI job fails).
- Every phase must pass locally: `go build ./...`, `go vet ./...`, `go test -race ./...` (CI runs `-race` even though the Makefile doesn't), and staticcheck.
- New endpoints stay on `x.com` only (SECURITY.md host allowlist: `x.com`, `upload.twitter.com`, `*.twimg.com`, `t.co`).
- Read-only requests need NO `X-Client-Transaction-Id` — use the plain `setHeaders` (`pkg/api/web.go:317`), never `setCreateTweetHeaders`.
- Commits small and logically separable (upstream churns daily in `internal/timeline/` and `pkg/api/`; rebase pain is expected). PR titles are the changelog (`goreleaser` uses `github-native`) — write them descriptively.
- **Never fabricate a GraphQL query ID.** All new operations use the TweetDetail pattern: empty fallback + `discoverOperation` on first use (`pkg/api/conversation.go:38` is the template).

---

## 1. Phase 0 — `following bool` → `FeedKind` enum (behavior-preserving)

**Goal**: replace the bare `following bool` feed identity with an exported enum so >2 feeds are representable. Zero behavior change. This commit is standalone and upstream-PR-able.

### 1.1 Type

Add to `internal/timeline/model.go`, next to the existing `ActionKind` exported-enum precedent (`model.go:42-50`):

```go
// FeedKind identifies which timeline the feed pane is showing.
type FeedKind int

const (
	FeedForYou FeedKind = iota
	FeedFollowing
	FeedBookmarks // added in Phase 2
	FeedSearch    // added in Phase 4
)
```

**Exported** — it must cross the `cmd` → `internal/timeline` package boundary (`cmd` passes the initial feed into `timeline.Run`). `ActionKind` is the existing precedent for exactly this. In Phase 0, only `FeedForYou`/`FeedFollowing` exist; add the other two constants in their own phases.

### 1.2 Model field

Replace `following bool` at `internal/timeline/model.go:66` with `feed FeedKind`.

### 1.3 Signature changes

- `timeline.Run` (`model.go:222`): `func Run(ctx context.Context, images string, following bool) (Action, error)` → `func Run(ctx context.Context, images string, feed FeedKind) (Action, error)`.
- `fetchPageSeq` (`model.go:255`): parameter `following bool` → `feed FeedKind`. Dispatch becomes a switch:

```go
fetch := client.FetchHomeTimeline
if feed == FeedFollowing {
	fetch = client.FetchFollowingTimeline
}
```

(Phase 1/3 extend this switch; keep it an if-chain or switch on `feed`, still assigning a `func(context.Context, string, int) (*api.TimelinePage, error)` value so `pageMsg` plumbing is untouched.)

### 1.4 Call sites (all 5 `following` uses)

- `model.go:219` (Init) — pass `m.feed`.
- `model.go:222` (`Run`) — store the param into the model as `feed`.
- `model.go:463` (`R` refresh) — `m.feed`.
- `model.go:645` (`switchFeed`) — see 1.5.
- `model.go:651` (`maybeLoadMore`) — `m.feed`.

### 1.5 `switchFeed` and the `f` key

**Decision: `f` stays the For You ⇄ Following toggle forever; new feeds get discrete keys** (`b` for bookmarks, `/` for search). Justification: `f` toggling between exactly two home flavors matches every user's muscle memory and the upstream UI; a 4-way cycle on one key makes reaching a specific feed take up to 3 presses and makes the Phase 0 diff non-neutral.

Refactor `switchFeed()` (`model.go:631-646`) into two functions:

```go
// setFeed resets feed state and kicks off a fresh first page for kind.
func (m *model) setFeed(kind FeedKind) tea.Cmd   // body = current switchFeed reset logic
                                                  // (posts/cursor/selected/expanded/loading
                                                  //  flags/viewport reset, feedSeq++)
                                                  // + m.feed = kind + fetchPageSeq(...)

// switchFeed keeps the f-key semantics: toggle ForYou <-> Following.
// From any other feed kind it returns to ForYou.
func (m *model) switchFeed() tea.Cmd {
	if m.feed == FeedForYou {
		return m.setFeed(FeedFollowing)
	}
	return m.setFeed(FeedForYou)
}
```

The `feedSeq` bump inside `setFeed` keeps the existing stale-page guard (`applyFeedPage`, `model.go:656-661`) working for free.

### 1.6 Header

`view.go:71-76`: replace the bool branch with a switch over `m.feed`: `FeedForYou → "for you"`, `FeedFollowing → "following"`. (Later phases add `"bookmarks"` and `"search: <query>"`.)

### 1.7 CLI

- `cmd/timeline.go:40`: `runTimeline(ctx context.Context, imageMode string, following bool)` → `runTimeline(ctx context.Context, imageMode string, feed timeline.FeedKind)`. Both callers map their `--following` bool: `feed := timeline.FeedForYou; if following { feed = timeline.FeedFollowing }`.
- The `--following` flag itself stays a bool at BOTH declaration sites (`cmd/timeline.go:35` and `cmd/root.go:88`) — no flag-surface change in Phase 0.
- `runTimeline`'s relaunch loop re-enters `timeline.Run` with the same `feed` value (unchanged behavior).

### 1.8 Verify

`make dev` green; manual: `xeet` opens for-you, `xeet --following` and `xeet timeline --following` open following, `f` toggles, `R` refreshes the current feed, infinite scroll still loads. `go test -race ./...` green.

---

## 2. Phase 1 — Bookmarks API

**Goal**: `client.FetchBookmarks(ctx, cursor, 30)` returns a `*TimelinePage`.

### 2.1 doTimeline parameterization

**Decision: refactor, don't clone.** Extract the request core of `doTimeline` (`pkg/api/timeline.go:163`) into a sibling that takes the variables map, and make `doTimeline` a thin wrapper:

```go
// doTimelineOp issues a GET timeline GraphQL request with caller-supplied variables.
// features/fieldToggles are the shared timelineFeatures / timelineFieldToggles maps.
func (c *WebClient) doTimelineOp(ctx context.Context, operation, qid string, variables map[string]any) (*httpResult, error)

func (c *WebClient) doTimeline(ctx context.Context, operation, qid, cursor string, count int) (*httpResult, error) {
	vars := map[string]any{ /* exactly the current hardcoded home map:
	   count, includePromotedContent, latestControlAvailable,
	   requestContext launch/scroll, seenTweetIds, withCommunity, cursor */ }
	return c.doTimelineOp(ctx, operation, qid, vars)
}
```

Justification: the endpoint format (`https://x.com/i/api/graphql/%s/%s?%s`, GET, params `variables`/`features`/`fieldToggles`), header setup, and `send` invocation are identical for every timeline read; only `variables` differs. One extraction beats three near-identical request builders, and the home path is byte-for-byte preserved.

Likewise, `fetchTimeline` (`timeline.go:114`) — the driver with the session guard / count clamp / QID lookup / refresh-retry / status handling / parse — gets a variables-aware sibling:

```go
func (c *WebClient) fetchTimelineOp(ctx context.Context, operation, fallback, environment string,
	buildVars func(count int) map[string]any) (*TimelinePage, error)
```

`buildVars` receives the already-clamped count. `fetchTimeline` becomes a wrapper that passes a `buildVars` closure producing the home variables (with its cursor captured). The retry-after-`discoverOperation` path must re-issue through the same `buildVars`. All existing behavior (clamp `count <= 0 || count > 100 → 30`, `needsQueryIDRefresh` retry, `statusToError`, `isTransientStatus`, `graphQLError`, `parseTimeline`) is inherited verbatim.

### 2.2 New file `pkg/api/bookmarks.go`

```go
const bookmarksOperation = "Bookmarks"

// FetchBookmarks returns a page of the authenticated user's bookmarks.
func (c *WebClient) FetchBookmarks(ctx context.Context, cursor string, count int) (*TimelinePage, error) {
	return c.fetchTimelineOp(ctx, bookmarksOperation, "", "XEET_BOOKMARKS_QID",
		func(count int) map[string]any {
			vars := map[string]any{
				"count":                  count,
				"includePromotedContent": false,
			}
			if cursor != "" {
				vars["cursor"] = cursor
			}
			return vars
		})
}
```

- Operation name: **`Bookmarks`** (X web client's bookmarks timeline GraphQL operation).
- Fallback QID: **`""`** — discover-on-first-use, exactly like TweetDetail (`conversation.go:38`). Never hardcode a QID constant.
- Environment override name: **`XEET_BOOKMARKS_QID`**. The convention is VERIFIED as `XEET_<OPERATION-UPPERCASED-NO-SEPARATORS>_QID` — see `timeline.go:106` (`XEET_HOMETIMELINE_QID`), `timeline.go:111` (`XEET_HOMELATESTTIMELINE_QID`), `conversation.go:38` (`XEET_TWEETDETAIL_QID`), `account.go:36` (`XEET_VIEWER_QID`), `web.go:188` (`XEET_CREATETWEET_QID`). Use it exactly as written above.
- Variables kept minimal deliberately: X's Bookmarks operation is tolerant of a minimal set; §8 defines the recovery procedure if a live call 400s.

### 2.3 QID plumbing — all 7 sites (exact names)

| Site | Edit |
|---|---|
| `pkg/config/config.go:26-32` | `BookmarksQID string \`yaml:"bookmarks_qid,omitempty"\`` on `Config` |
| `pkg/config/config.go:98-104` | same field on `fileConfig` |
| `pkg/config/config.go:142-148` | fileConfig→Config copy line |
| `pkg/config/config.go:244-247` | reverse copy site 1 |
| `pkg/config/config.go:281-287` | reverse copy site 2 |
| `pkg/api/web.go:175-183` | `"Bookmarks": cfg.BookmarksQID,` in the `operationQIDs` literal |
| `pkg/api/web.go:243-258` | `case "Bookmarks": cfg.BookmarksQID = qid` in `ApplyRefreshedQueryIDs` |

`operationHint` (`pkg/api/discover.go:105`): `"Bookmarks"` contains neither "Home" nor "Tweet" → falls through to `return operation`, which is correct. No edit needed here for bookmarks (the Search fix lands in Phase 3).

### 2.4 Verify

Unit: mocked-server test (see Phase 6). Live smoke (optional at this phase, mandatory before PR): the Phase 6 live test with `XEET_LIVE_BOOKMARKS=1`.

---

## 3. Phase 2 — Bookmarks TUI + CLI

### 3.1 TUI

- Add `FeedBookmarks` to the `FeedKind` consts.
- `fetchPageSeq` dispatch: `case FeedBookmarks: fetch = client.FetchBookmarks`. (The `ApplyRefreshedQueryIDs` + `mgr.Save(cfg)` line after the fetch already persists a discovered QID — no extra work.)
- Key binding: **`b`** in `modeFeed` key handling (same block that handles `f`): `return m, m.setFeed(FeedBookmarks)` — except when already there, `b` returns home: `if m.feed == FeedBookmarks { return m, m.setFeed(FeedForYou) }`. Justification: a toggle-back is the cheapest escape hatch and mirrors `f`'s toggle feel. Check `b` is currently unbound in `modeFeed` before choosing; if bound, use `B`.
- Header (`view.go:71-76` switch): `FeedBookmarks → "bookmarks"`.
- `switchFeed` (`f`) from bookmarks already returns to `FeedForYou` per Phase 0's rule.
- Update the help overlay text (`help` view) with the new binding.
- `maybeLoadMore` / `R` refresh / `feedSeq` guard: all work unchanged — bookmarks is just another feed kind flowing through `pageMsg`. **Zero new message types.**

### 3.2 CLI — flag in BOTH places (critical)

- `cmd/timeline.go`: add `timelineBookmarks bool` var + `--bookmarks` flag in `init()`; `RunE` maps flags → `FeedKind` (`--bookmarks` wins over `--following`; but see next line).
- `cmd/root.go`: add `rootBookmarks` + `--bookmarks` on `rootCmd` (`:88` area) — without this `xeet --bookmarks` breaks while `xeet timeline --bookmarks` works.
- Mutual exclusions (`cmd/root.go:90-91` style, and the equivalent on `timelineCmd`):
  - `MarkFlagsMutuallyExclusive("following", "bookmarks")` on both commands
  - `MarkFlagsMutuallyExclusive("compose", "bookmarks")` on root (matching the existing `("compose", "following")`)

### 3.3 Verify

Manual: `xeet --bookmarks`, `xeet timeline --bookmarks`, `b` toggle in-TUI, infinite scroll past 30, `R` refresh shows the prepend/dedup toast path. `--following --bookmarks` errors out. `go test -race ./...`.

---

## 4. Phase 3 — Search API

### 4.1 Entry walker: lift, share, leave parseConversation alone

**Decision: add a shared `parseEntries` walker modeled on `parseConversation`'s (handles `entries []any`, `content → itemContent`, `content.items[].item.itemContent` module expansion, bottom/showmore cursors), use it from `parseTimeline`, and DO NOT touch `parseConversation`.**

Justification: SearchTimeline commonly nests tweets inside module entries (conversation clusters in "Top" results) which `parseTimeline`'s blind walk misses entirely — so module handling is a correctness requirement for search, not a nice-to-have. But rewriting `parseConversation` to consume the shared helper churns a file upstream edits daily for zero behavior gain; duplication of ~40 lines of walking logic is the cheaper ongoing cost. Concretely:

- New unexported func in `pkg/api/timeline.go`: `func parseEntries(payload any) (posts []TimelinePost, bottomCursor string)` — walk to maps holding an `entries []any` key; per entry handle direct `content.itemContent`, module `content.items[].item.itemContent` (each through `parseTimelineItem`), and cursor entries whose `cursorType` lower-cased equals `"bottom"` or contains `"showmore"`.
- `parseTimeline` becomes a thin wrapper assembling `TimelinePage` from `parseEntries` output. This is behavior-preserving-or-better for Home/Following/Bookmarks (it additionally picks up module-nested tweets and preserves entry order). Existing `parseTimeline` tests must still pass; if any asserts the old blind-walk quirks, update the assertion and say why in the test.
- `parseTimelineItem` (`timeline.go:227`) is reused unchanged; it already skips `promoted_metadata`, which also handles promoted search results.

### 4.2 New file `pkg/api/search.go`

```go
const searchOperation = "SearchTimeline"

// FetchSearchTimeline returns a page of search results for query.
func (c *WebClient) FetchSearchTimeline(ctx context.Context, query, cursor string, count int) (*TimelinePage, error) {
	return c.fetchTimelineOp(ctx, searchOperation, "", "XEET_SEARCHTIMELINE_QID",
		func(count int) map[string]any {
			vars := map[string]any{
				"rawQuery":    query,
				"count":       count,
				"querySource": "typed_query",
				"product":     "Top",
			}
			if cursor != "" {
				vars["cursor"] = cursor
			}
			return vars
		})
}
```

- Operation name: **`SearchTimeline`**.
- `product`: **`"Top"`** (X's default tab; "Latest" is a plausible v2 flag but out of scope — do not add a flag now).
- `querySource`: `"typed_query"` (what the web client sends for user-typed searches).
- Fallback QID `""`, discover-on-first-use. Environment override name: **`XEET_SEARCHTIMELINE_QID`** (convention verified in §2.2).
- Empty `query` must return an error before any network call: `if strings.TrimSpace(query) == "" { return nil, errors.New("search query is empty") }` (style-match the package's existing error wording).

### 4.3 `operationHint` bug fix — `pkg/api/discover.go:105`

`"SearchTimeline"` contains `"Tweet"` → the current code wrongly returns `"Compose"`, so discovery would scan the wrong bundle. Insert an explicit case BEFORE the `strings.Contains(operation, "Tweet")` line:

```go
if operation == "SearchTimeline" {
	return "SearchTimeline"
}
```

Add a unit test for `operationHint` covering `TweetDetail`, `HomeTimeline`, `CreateTweet`, `SearchTimeline`, `Bookmarks` expectations.

### 4.4 QID plumbing — same 7 sites as §2.3

Field `SearchTimelineQID string` with yaml tag `search_timeline_qid,omitempty`; map key / switch case string `"SearchTimeline"`.

### 4.5 Verify

Mocked test including a module-nested fixture (Phase 6). `go test -race ./...`.

---

## 5. Phase 4 — Search prompt mode (TUI)

### 5.1 Query threading — closure capture

**Decision: keep the fetcher signature fixed and capture the query in a closure inside `fetchPageSeq`.** Justification: widening the shared fetcher signature to take a query would ripple through every feed's dispatch and `pageMsg` for a parameter three of four feeds ignore; a closure is local and free.

- Model gains `searchQuery string`.
- `fetchPageSeq` gains the query as an explicit parameter (so the goroutine doesn't read model state): call sites pass `m.searchQuery`. Dispatch:

```go
case FeedSearch:
	q := query
	fetch = func(ctx context.Context, cursor string, count int) (*api.TimelinePage, error) {
		return client.FetchSearchTimeline(ctx, q, cursor, count)
	}
```

### 5.2 Mode machinery (clone the reply pattern, flat enum)

- `model.go:52-58`: add `modeSearch` to the `mode` consts.
- Model fields (next to `replyEditor` / `replyReturn`):
  - `searchInput textinput.Model` — configured where `replyEditor` is configured (`model.go:194-198`): placeholder `"search"`, char limit 512, focused on open. `bubbles/textinput` is already a dependency — no go.mod change.
  - `searchReturn mode` — return pointer, same single-field pattern as `replyReturn` (`model.go:103`).
  - `searchPrevFeed FeedKind` — the feed to restore on cancel (esc must not dump a thread-viewer or a bookmarks user into for-you).
- Dispatch: add `case modeSearch: return m.updateSearch(msg)` at the mode dispatch (`model.go:388-393`). Overlay bools (`help`/`altText`/`zoom`) stay checked before mode dispatch — unchanged.
- `View()`: add `viewSearch()` at the same precedence position as `viewReply` (`view.go:19-33` ordering).

### 5.3 Opening the prompt

**Key: `/`** (universal search convention; unbound in feed mode today — verify, fall back to `s` if taken). Allowed from **`modeFeed` and `modeThread`**:

```go
func (m *model) beginSearch() {           // clone of beginReply's shape (reply.go:40-49)
	m.searchReturn = m.mode
	m.searchPrevFeed = m.feed
	m.searchInput.SetValue(m.searchQuery) // reopening pre-fills current query
	m.searchInput.Focus()
	m.mode = modeSearch
}
```

### 5.4 `updateSearch` (new file `internal/timeline/search.go`, modeled on `reply.go:51-138`)

- **esc**: `m.mode = m.searchReturn`, blur input, restore nothing else (feed untouched — canceling never changes the feed). Mirror `reply.go:113-119`.
- **enter**:
  - `q := strings.TrimSpace(m.searchInput.Value())`
  - **Empty query → treat as cancel** (same as esc). Justification: an empty search has no meaningful result; silently staying in the prompt punishes an accidental enter.
  - Non-empty: `m.searchQuery = q; m.mode = modeFeed; return m, m.setFeed(FeedSearch)`. (`setFeed` bumps `feedSeq`, so any in-flight page from the previous feed is dropped by the stale guard.) Submitting from `modeThread` intentionally leaves the thread — search results are a feed.
- **fallthrough**: the canonical delegate,

```go
var cmd tea.Cmd
m.searchInput, cmd = m.searchInput.Update(msg)
return m, cmd
```

### 5.5 `viewSearch` (in `view.go`, modeled on `viewReply` at `view.go:515-561`)

Single-line prompt panel: title `"search"`, the textinput, hint line `"enter: search · esc: cancel"`. Reuse `viewReply`'s framing/lipgloss styles.

### 5.6 Required integration edits

- **`applyFeedPage` `model.go:670`**: the prompt can be open while a background page (infinite-scroll continuation) arrives, so extend the disjunction:

```go
threadContext := m.mode == modeThread ||
	(m.mode == modeReply && m.replyReturn == modeThread) ||
	(m.mode == modeSearch && m.searchReturn == modeThread)
```

- Header (`view.go` switch): `FeedSearch → "search: " + m.searchQuery` (truncate query to fit; reuse existing width-truncation helper if present).
- `f` from `FeedSearch` → `FeedForYou` (Phase 0 rule, already handled). `b` from search → bookmarks. `/` from search reopens the prompt pre-filled.
- `syncViewport` / `activePosts` / `currentPost` (`model.go:780-838`) branch only on `modeThread` — **unaffected**, because search results are a FEED KIND, not a viewing mode. This is the core design choice of this spec: only the *prompt* is a mode; the *results* are `m.posts`.
- `maybeLoadMore` works unchanged (cursor-based, feed-agnostic). `R` refresh in `FeedSearch` re-runs the same query from the top.
- Help overlay: document `/` and `b`.

### 5.7 Initial query from CLI

`timeline.Run` gains the query: `func Run(ctx context.Context, images string, feed FeedKind, query string) (Action, error)`. When `feed == FeedSearch && query != ""`, seed `m.searchQuery` before Init so the first `fetchPageSeq` searches immediately. When `feed == FeedSearch && query == ""`, open the search prompt on start (`beginSearch` from Init) over an empty feed.

### 5.8 Verify

Manual script: `/` → type → enter → results render, header shows query; scroll loads page 2; `/` re-opens pre-filled; esc cancels without feed change; empty enter cancels; open a thread from a search result and return; `R` re-searches. `go test -race ./...`.

---

## 6. Phase 5 — `cmd/search.go`

New file following the `post.go` / `theme.go` cobra template:

```go
var (
	searchImageMode string
	searchTheme     string
)

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search posts and browse results",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := applyConfiguredTheme(searchTheme); err != nil { return err }
		query := ""
		if len(args) == 1 { query = args[0] }
		return runTimeline(cmd.Context(), searchImageMode, timeline.FeedSearch, query)
	},
}

func init() {
	rootCmd.AddCommand(searchCmd)
	// register --images/--theme flags exactly as cmd/timeline.go does
}
```

- `runTimeline` (widened in this phase or Phase 4, whichever compiles first — keep it one commit): `func runTimeline(ctx context.Context, imageMode string, feed timeline.FeedKind, query string) error`. Existing callers pass `""`. The relaunch loop (`cmd/timeline.go:40-83`) re-enters with the same feed+query after `ActionCompose`/`ActionAuthenticate`, preserving current behavior.
- `xeet search` with no args opens the TUI with the search prompt focused (§5.7). Quoted multi-word queries arrive as one arg; that's the documented usage (`xeet search "foo bar"`).
- No `--bookmarks`/`--following` flags on `searchCmd` — it is single-purpose.

Verify: `xeet search "golang"` lands directly in results; `xeet search` opens the prompt; `xeet --bookmarks` still works (regression check on the shared `runTimeline`).

---

## 7. Phase 6 — Tests

Mocked offline tests are the bar (repo default: 42 test files; `pkg/api` alone has 20). Author fixtures **by hand** — there is no recorded-fixture infrastructure. Follow the existing `pkg/api` mocked-server test style (httptest server injected as the client's base URL; read a neighboring test for the exact harness before writing).

### 7.1 Minimal fixture shapes

The parsers only need: a nesting that leads to an `entries` array; per tweet an `itemContent` holding a `tweet_results` shaped like the existing fixtures feed `parseTimelineItem`; and a cursor entry. Copy a tweet blob from an existing timeline test fixture rather than inventing one. Skeletons:

**Bookmarks** (`testdata` or inline const, matching neighbors):

```json
{"data":{"bookmark_timeline_v2":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[
  {"entryId":"tweet-1","content":{"entryType":"TimelineTimelineItem","itemContent":{ /* copied tweet_results blob */ }}},
  {"entryId":"cursor-bottom-1","content":{"entryType":"TimelineTimelineCursor","cursorType":"Bottom","value":"CURSOR_B1"}}
]}]}}}}
```

**Search** — must include a module entry to prove `parseEntries` descends into `content.items[]`:

```json
{"data":{"search_by_raw_query":{"search_timeline":{"timeline":{"instructions":[{"type":"TimelineAddEntries","entries":[
  {"entryId":"tweet-2","content":{"entryType":"TimelineTimelineItem","itemContent":{ /* tweet blob A */ }}},
  {"entryId":"search-conversation-1","content":{"entryType":"TimelineTimelineModule","items":[
    {"entryId":"search-conversation-1-tweet-3","item":{"itemContent":{ /* tweet blob B */ }}}
  ]}},
  {"entryId":"cursor-bottom-2","content":{"entryType":"TimelineTimelineCursor","cursorType":"Bottom","value":"CURSOR_S1"}}
]}]}}}}}
```

(The parsers are blind walkers — the exact top-level key names above don't need to match X's real response for the mocked tests to be valid; but use these anyway so the fixtures double as documentation of the expected real shape.)

### 7.2 Mocked tests to add

`pkg/api/bookmarks_test.go`:
1. **Happy path** — server returns the fixture; assert posts count, first post ID/text, `Cursor == "CURSOR_B1"`; assert the request URL contains `/i/api/graphql/` + the operation name, and decode the `variables` query param to assert `count` and `includePromotedContent:false`, no `cursor` key on first page.
2. **Cursor pass-through** — call with `cursor:"abc"`, assert `variables.cursor == "abc"`.
3. **Count clamp** — call with `count:0` and `count:500`, assert `variables.count == 30`.
4. **Empty-session guard / non-200 → `statusToError`** — mirror whichever of these neighboring timeline tests cover; don't re-test `fetchTimeline` internals exhaustively, just prove the wrapper is wired through it.

`pkg/api/search_test.go`:
1. **Happy path with module** — fixture above; assert BOTH tweets parsed (module descent) in entry order, cursor captured.
2. **Variables** — assert `rawQuery`, `product:"Top"`, `querySource:"typed_query"`.
3. **Empty query** — returns error, zero HTTP requests (assert via server hit counter).

`pkg/api/discover_test.go` (or wherever `operationHint` tests live): the 5-case table from §4.3 — this is the regression test for the Compose-hint bug.

`pkg/api/timeline_test.go`: existing `parseTimeline` tests must pass against the `parseEntries` rewrite; add one case proving order preservation + module descent for a home-shaped payload.

`internal/timeline`: follow whatever model-level test conventions exist there (if the package currently has no bubbletea unit tests, do not invent a harness — the TUI phases are covered by the manual scripts in §5.8/§6 and compile/vet/race).

### 7.3 Live tests (2 new files)

- `pkg/api/bookmarks_live_test.go` — gate: `if os.Getenv("XEET_LIVE_BOOKMARKS") != "1" { t.Skip("set XEET_LIVE_BOOKMARKS=1 to run") }`
- `pkg/api/search_live_test.go` — gate: `XEET_LIVE_SEARCH`, same pattern.

Both: per-file env-var gate (NOT a build tag), load a real session via `config.NewConfigManager()`, `context.WithTimeout(ctx, 40*time.Second)`, fetch one page (search query: `"golang"`), assert `len(posts) > 0` and cursor non-empty, then fetch page 2 with the cursor. Must **compile** on all 4 CI platforms (they will, being plain Go — just don't use PTY helpers).

### 7.4 Gate

`go build ./...`, `go vet ./...`, `go test -race ./...`, staticcheck, go-licenses (no new deps → trivially green), `nix build .#default` (no vendorHash change expected).

---

## 8. Open questions / risk register — what Codex must NOT guess

1. **GraphQL variables are UNVERIFIED.** The variable sets in §2.2/§4.2 are the best-known web-client shapes, but this investigation did not capture a live request. X's GraphQL returns 400 with a JSON body naming missing variables/features when the set is wrong. Procedure if the live test 400s:
   - Log/print the full response body (the `graphQLError`/`statusToError` path should surface it; if not, temporarily dump `httpResult.body` in the live test).
   - The error body lists the missing variable or feature name — add exactly what it names to the `buildVars` map (or to a per-operation features delta if it's a `features` complaint), re-run, repeat.
   - **`xeet inspect-har` EXISTS** (`cmd/inspecthar.go`) and is the intended tool here. Record a HAR in the browser while loading x.com/i/bookmarks and while running a search, then run `xeet inspect-har file.har`. Note its deliberate limitation: it prints **names and structural keys only — never values**, so it will tell you *which* variables the real request carries but not what they are set to. For the values, read the decoded `variables` query param directly from DevTools → Network → the `Bookmarks` / `SearchTimeline` XHR. Keep the HAR local; it contains session cookies.
   - Whatever you converge on, leave a code comment listing which variables were error-driven additions, and a `TODO(human): verify variables against a fresh HAR with a real session` if you could not run the live tests yourself (no session available in CI/sandbox).
2. **Never fabricate query IDs.** Fallback stays `""`; `discoverOperation` owns QID acquisition. If discovery fails for `Bookmarks`/`SearchTimeline`, that is a bug in `operationHint`/discovery scoping to investigate — not a cue to paste a QID from the internet (they rotate; a stale hardcode is worse than a discovery failure).
3. **Response envelope key names** (`bookmark_timeline_v2`, `search_by_raw_query`) are believed-correct but unverified; harmless either way since the walkers are key-blind. Do not add code that depends on these exact keys.
4. **`b` and `/` key availability** in `modeFeed` is asserted-unbound but re-check the key handling block before binding; fallbacks: `B`, `s`.
5. **Upstream churn**: before starting, rebase on the fork's current main; if `switchFeed`/`fetchPageSeq` moved, the line numbers in this spec are anchors, not gospel — the named functions are the contract.
6. **Rate limiting**: Bookmarks and SearchTimeline have their own rate buckets; `isTransientStatus`/`statusToError` handling is inherited from `fetchTimeline` — do not add bespoke retry logic.

---

## 9. Commit plan (PR titles = changelog)

1. `refactor(timeline): introduce FeedKind enum in place of following bool`
2. `feat(api): add Bookmarks timeline fetch with discover-on-first-use query id`
3. `feat(timeline): bookmarks feed (b key, --bookmarks flag)`
4. `feat(api): add SearchTimeline fetch, shared entry walker, operationHint fix`
5. `feat(timeline): search prompt mode (/ key)`
6. `feat(cmd): add search subcommand`
7. `test(api): mocked + live coverage for bookmarks and search`

(Fold 7 into 2/4 if the phase already ships its own tests — preferred; keep the split only if fixture authoring lags.)
