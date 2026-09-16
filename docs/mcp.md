# xeet mcp: read-only X search and bookmarks over MCP

`xeet mcp serve` exposes three read-only tools to any Model Context Protocol
client (ChatGPT through a Secure MCP Tunnel, Claude Code, Codex, an editor)
over stdio. It reuses the browser session `xeet auth` saved, so there is no
API key, and it can only read.

```
search_x_posts(query, account_id?, limit?, cursor?, timeout_seconds?)
get_x_bookmarks(account_id?, limit?, cursor?, timeout_seconds?)
get_x_session_health(account_id?, timeout_seconds?)
```

Nothing in `pkg/mcp` can reach a mutation: the package talks to the X client
through a three-method interface (search, bookmarks, viewer), and the tool
list *is* the allowlist. Posting, liking, bookmarking, reposting, following
and messaging are not registered and cannot be called.

## Running it

```bash
xeet mcp serve --allow-account @alice            # one account: account_id may be omitted
xeet mcp serve --allow-account 1234 --allow-account @bob   # several: account_id is required
XEET_MCP_ALLOWED_ACCOUNTS=@alice xeet mcp serve  # same, for launchd / systemd units
```

| flag | default | cap | meaning |
|---|---|---|---|
| `--allow-account` | (none: refuses to start) | | saved accounts the tools may read as; handle or user id |
| `--timeout` | 30s | 60s | bound for one search or bookmarks call |
| `--health-timeout` | 15s | 60s | bound for the live check in `get_x_session_health` |
| `--max-pages` | 3 | 10 | upstream pages one call may fetch |
| `--page-delay` | 1s | | pause between pages inside one call |
| `--max-concurrent` | 1 | | calls in flight; others wait 5s then get `BUSY` |

stdout carries the protocol only. Diagnostics are JSON lines on stderr
(tool, account id, status, code, count, duration) and never include the
query text at info level, cookies, or headers.

The server never writes: it does not persist rediscovered GraphQL query ids
or the viewer identity, so it cannot rewrite `~/.xeet.yaml` while the
interactive `xeet` also owns it. Re-run `xeet whoami` occasionally to keep the
cached ids fresh if you want to avoid the rediscovery cost.

### Local checks without an MCP client

```bash
xeet mcp tools --allow-account @alice                       # tools/list, as JSON
xeet mcp call get_x_session_health --allow-account @alice   # exit 0 ok, 1 error
xeet mcp call search_x_posts --allow-account @alice --query "go tui" --limit 5
xeet mcp call get_x_bookmarks  --allow-account @alice --limit 5 --cursor "$NEXT"
```

`call` runs a real MCP client against the server over an in-memory pipe, so
it exercises initialize, tools/list and tools/call, not the handler directly.
Exit status: 0 for `ok` or `empty`, 2 for `partial`, 1 for `error`.

## Account selection

`account_id` is matched against `--allow-account` first, then against the
accounts saved by `xeet auth` (`xeet accounts` lists them). It accepts a
numeric X user id or a handle with or without `@`, case-insensitively.

- one allowed account and no `account_id` → that account
- several allowed and no `account_id` → `ACCOUNT_REQUIRED`
- `account_id` not in the allowlist (even if saved on the machine) → `ACCOUNT_MISMATCH`
- allowed but not saved → `AUTH_REQUIRED`

There is no fallback to the active account, and the health tool confirms
with X that the saved session really belongs to the requested user id
(`account_match`).

## Output contract

Every result is structured (`structuredContent`, mirrored as JSON text in
`content`) and carries `status`, `tool`, `fetched_at`, and the account it
was read as. Failures set `isError: true` and put the reason in `error`.

### search_x_posts / get_x_bookmarks

```json
{
  "status": "ok",
  "tool": "search_x_posts",
  "source": "x.com SearchTimeline (live, product=Top)",
  "freshness": "live",
  "account_id": "1234",
  "account_handle": "alice",
  "query": "go tui",
  "fetched_at": "2026-09-16T02:30:38Z",
  "requested_limit": 3,
  "count": 3,
  "pages_fetched": 1,
  "complete": true,
  "has_more": true,
  "next_cursor": "eyJ2IjoxLCJ0Ijoic2VhcmNo…",
  "newest_created_at": "2026-08-01T14:34:08Z",
  "oldest_created_at": "2024-04-20T13:47:32Z",
  "posts": [
    {
      "id": "2079416420493103335",
      "url": "https://x.com/golangch/status/2079416420493103335",
      "text": "An interesting article and project: …",
      "author": {"name": "Golang", "handle": "golangch", "url": "https://x.com/golangch"},
      "created_at": "2026-07-21T04:01:22Z",
      "metrics": {"replies": 0, "reposts": 5, "likes": 24, "views": 4409},
      "media_count": 0
    }
  ]
}
```

- `source` says where the data came from. Bookmarks are read live from x.com,
  not from any local database.
- `fetched_at` is when this call ran; `created_at` (and the
  newest/oldest range) is the post's own timestamp. They are never conflated.
- `metrics.*` is `null` when X did not include that counter, which is
  different from `0`.
- `complete` means every page this call attempted was fetched without error.
- `has_more` / `next_cursor` mean X has a further page. They say nothing
  about failure.
- `status`: `ok` (posts, no failure), `empty` (no posts, no failure),
  `partial` (a later page failed; `complete=false`, `error.code=PARTIAL_RESULT`,
  `error.cause` names the failure class, `next_cursor` resumes after the last
  good page), `error` (nothing usable; `isError=true`).
- `note` explains a benign stop: the page cap, or a page that held more
  posts than `limit`.

Cursors are opaque and bound to the tool, account and query that issued
them. Reusing one anywhere else is refused with `INVALID_CURSOR`. They
contain X's pagination cursor only, never session material.

### get_x_session_health

```json
{
  "status": "ok",
  "tool": "get_x_session_health",
  "fetched_at": "2026-09-16T02:30:36Z",
  "account_id": "1234",
  "account_handle": "alice",
  "allowed_accounts": ["1234"],
  "configured": true,
  "config_version": 2,
  "keyring": "ok",
  "session_source": "Chrome / Default",
  "cookie_expires_at": "2027-05-29T08:24:48Z",
  "cookie_expired_locally": false,
  "imported_at": "2026-08-06T02:09:36Z",
  "connection": "authenticated",
  "checked_at": "2026-09-16T02:30:37Z",
  "last_authenticated_at": "2026-09-16T02:30:37Z",
  "viewer": {"id": "1234", "handle": "alice", "name": "Alice"},
  "account_match": true
}
```

`keyring` is `ok`, `incomplete`, `unavailable` or `not_checked`.
`connection` is `authenticated`, `auth_required`, `rate_limited`, `timeout`,
`unreachable`, `upstream_changed`, `upstream_error` or `not_checked`. The
check is one authenticated `Viewer` read; it never logs in, imports cookies,
or opens a browser. `last_authenticated_at` is per server process.

### Error codes

| code | meaning | fix |
|---|---|---|
| `INVALID_ARGUMENT` | query empty/too long/control chars, limit outside 1–100, timeout outside 1–60 | fix the call |
| `INVALID_CURSOR` | cursor not issued by this server, or issued for another tool/account/query | page with the cursor you were given |
| `NOT_CONFIGURED` | server started without any allowed account | start with `--allow-account` |
| `ACCOUNT_REQUIRED` | several accounts allowed, none named | pass `account_id` |
| `ACCOUNT_MISMATCH` | account not allowed, or the saved session belongs to another user | pass an allowed account / re-run `xeet auth` |
| `AUTH_REQUIRED` | no saved session, incomplete keyring pair, or X rejected the cookies | `xeet auth` |
| `KEYRING_UNAVAILABLE` | the OS keyring could not be read (locked keychain, no Secret Service) | unlock the keychain / run where a keyring exists |
| `RATE_LIMITED` | HTTP 429 or GraphQL code 88; `retry_after_seconds` when X said | wait |
| `TIMEOUT` | the call or a page exceeded its bound | retry, raise `timeout_seconds` (≤60) |
| `UPSTREAM_CHANGED` | query id rotated and rediscovery failed, or the response shape is unknown | update xeet |
| `UPSTREAM_UNAVAILABLE` | X 5xx after retries | retry later |
| `UPSTREAM_ERROR` | any other unexpected X response | see `message` |
| `NETWORK_ERROR` | DNS/connect failure | check connectivity |
| `PARTIAL_RESULT` | a page after the first failed; see `cause` | resume with `next_cursor` |
| `BUSY` | concurrency cap reached | retry shortly |

## Where to run it: Mac, not the VPS

The session lives in the OS keyring (macOS Keychain or Linux Secret
Service) and was imported from a browser on the same machine. The shared
VPS has D-Bus but no Secret Service provider (`gnome-keyring-daemon` and
`secret-tool` are absent), no browser to import from, and a datacenter IP
that X may treat differently from the residential one the cookies were
issued to. Running headless gnome-keyring on the VPS and copying cookies
across would work around all three, but every one of those workarounds is
a new way for the session to break silently. So:

- **the server runs on the Mac that ran `xeet auth`**, under launchd, and
- **ChatGPT reaches it through OpenAI's Secure MCP Tunnel**, whose client
  makes only outbound HTTPS from the Mac. No inbound port, no public URL.

The trade-off is availability: the tunnel is up while the Mac is awake and
logged in (the Keychain must be unlocked). For a personal research tool that
is acceptable; if it stops being acceptable, the Linux path is to provide
Secret Service on the VPS and re-run `xeet auth` there with cookies exported
manually, and the `KEYRING_UNAVAILABLE` code is what tells you it is not set
up yet.

Linux builds are verified in CI (`GOOS=linux` amd64/arm64, `CGO_ENABLED=0`),
so the binary itself is not the blocker.

## Deployment on the Mac (private, ChatGPT via Secure MCP Tunnel)

Files under `docs/mcp/` are templates; copy and edit, do not run in place.

1. Build and install the binary from this branch's pinned commit:

   ```bash
   git -C ~/dev/xeet log -1 --format=%H          # record this as the source commit
   make -C ~/dev/xeet install                   # /usr/local/bin/xeet
   xeet version -v
   ```

2. Confirm the session once, interactively (this is also the moment macOS
   grants the new binary Keychain access if it asks):

   ```bash
   xeet accounts
   xeet mcp call get_x_session_health --allow-account <user-id>
   ```

3. Create the Tunnel on `platform.openai.com/settings/organization/tunnels`
   (attach it to the ChatGPT workspace) and a *restricted* API key with
   Tunnels Read+Use only. Put the key in an env file the launchd job reads;
   never in the plist, the profile, or git:

   ```bash
   mkdir -p ~/.config/xeet-mcp && umask 077
   secret get OPENAI_TUNNEL_CONTROL_PLANE_API_KEY | sed 's/^/CONTROL_PLANE_API_KEY=/' > ~/.config/xeet-mcp/tunnel.env
   echo "CONTROL_PLANE_TUNNEL_ID=<tunnel_id>" >> ~/.config/xeet-mcp/tunnel.env
   chmod 600 ~/.config/xeet-mcp/tunnel.env
   ```

4. Fetch the full `tunnel-client` CLI (not the `-runtime-` build) for
   `darwin-arm64`, verify it against `SHA256SUMS.txt`, and initialise a
   profile whose MCP command is the server:

   ```bash
   ~/.local/bin/tunnel-client init --sample sample_mcp_stdio_local \
     --profile xeet --profile-dir ~/.config/xeet-mcp/tunnel-profile \
     --tunnel-id "$CONTROL_PLANE_TUNNEL_ID" \
     --mcp-command "/usr/local/bin/xeet mcp serve --allow-account <user-id>" \
     --control-plane-api-key-ref env:CONTROL_PLANE_API_KEY \
     --health-listen-addr 127.0.0.1:0
   ~/.local/bin/tunnel-client doctor --profile xeet --profile-dir ~/.config/xeet-mcp/tunnel-profile --explain
   ```

   Every doctor item must PASS before going further.

5. Install the launchd job from `docs/mcp/com.xeet.mcp-tunnel.plist` and
   `docs/mcp/tunnel-start.sh`:

   ```bash
   cp docs/mcp/tunnel-start.sh ~/.config/xeet-mcp/ && chmod 700 ~/.config/xeet-mcp/tunnel-start.sh
   cp docs/mcp/com.xeet.mcp-tunnel.plist ~/Library/LaunchAgents/
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.xeet.mcp-tunnel.plist
   launchctl print gui/$(id -u)/com.xeet.mcp-tunnel | head
   tail -f ~/Library/Logs/xeet-mcp-tunnel.log     # expect "tunnel-client started" and "tunnel metadata fetched"
   curl "$(cat ~/.config/xeet-mcp/tunnel-profile/health-url)/readyz"
   ```

6. In ChatGPT: Settings → Apps → Advanced → Developer Mode on → Create →
   Connection: **Tunnel** → pick the tunnel → **Scan Tools** must list exactly
   `get_x_bookmarks`, `get_x_session_health`, `search_x_posts` → Create. The
   app then shows under Enabled Apps with a Dev label; mention it with `@`
   in a new chat.

### Restart / rollback

```bash
launchctl kickstart -k gui/$(id -u)/com.xeet.mcp-tunnel      # restart after a binary or profile change
launchctl bootout gui/$(id -u)/com.xeet.mcp-tunnel           # stop; ChatGPT loses the tunnel, nothing else changes
git -C ~/dev/xeet checkout <previous-commit> && make -C ~/dev/xeet install && launchctl kickstart -k gui/$(id -u)/com.xeet.mcp-tunnel
```

Rolling back the binary is enough: the server writes nothing, so there is
no state to undo. Removing the ChatGPT app or the Tunnel on
platform.openai.com revokes access from that side.

### Session initialisation and refresh

```bash
xeet auth                                   # import from the browser; names the account
xeet accounts                               # see what is saved, which user id to allow
xeet mcp call get_x_session_health --allow-account <user-id>
```

When the health tool reports `AUTH_REQUIRED` or `account_match=false`, log
in to x.com in the browser and re-run `xeet auth` for that account. The
server picks up the new cookie pair on the next call; no restart needed.
When it reports `KEYRING_UNAVAILABLE` on the Mac, the Keychain is locked
(log in, or `security unlock-keychain`).

## Other MCP clients

Claude Code, Codex or any stdio client can use the same command; see
`docs/mcp/mcp.json`. Only the tools above are exposed regardless of client.

## Three questions to confirm the ChatGPT end to end

1. "Check my X session health" → a `get_x_session_health` call reporting
   `connection: authenticated` and `account_match: true` for the allowed id.
2. "Search X for `golang tui` and give me the 3 most relevant posts with
   their like counts" → `search_x_posts` with `limit: 3`, three posts with
   URLs and numeric (or null) likes, `has_more: true`.
3. "Show my 5 latest X bookmarks, then the next 5" → `get_x_bookmarks`
   with `limit: 5`, then a second call passing the returned `next_cursor`,
   with no overlapping ids between the two.

A useful negative check: ask it to like or repost one of the results. The
correct answer is that no such tool exists.
