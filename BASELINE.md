# Baseline state — READ THIS BEFORE IMPLEMENTING

Recorded 2026-07-27 on fork `rrrrnmtsu/xeet` at upstream commit `b00920f` ("mehh"),
i.e. **before any of our changes**.

## Toolchain

- Go 1.26.5 (matches the `go 1.26.5` pin in go.mod) installed via Homebrew at
  `/opt/homebrew/bin/go`. If `go` is not on PATH, use
  `export PATH="/opt/homebrew/bin:$PATH"`.

## `go build ./...`

Passes clean.

## `go test -race ./...` — TWO PRE-EXISTING FAILURES

```
--- FAIL: TestPreviewsPrefetchAroundSelection (0.00s)
    preview_test.go:380: prefetched 10 previews
--- FAIL: TestUnselectedPostNearSelectionRendersCachedImage (0.00s)
    view_test.go:93: image outside the inline radius rendered inline
FAIL	github.com/melqtx/xeet/internal/timeline	6.975s
```

Every other package passes:

```
ok  	github.com/melqtx/xeet/cmd
ok  	github.com/melqtx/xeet/internal/media
ok  	github.com/melqtx/xeet/internal/theme
ok  	github.com/melqtx/xeet/internal/tui
ok  	github.com/melqtx/xeet/internal/ui
ok  	github.com/melqtx/xeet/pkg/api
ok  	github.com/melqtx/xeet/pkg/config
```

## What this means for the implementation

**These two failures are NOT yours. Do not try to fix them.** They are in
`internal/timeline`'s image-preview radius logic and fail on unmodified upstream
code in this environment.

Your acceptance criterion is therefore:

> `go test -race ./...` shows **exactly these two failures and no others**.

If a third failure appears, that one is yours. If either of these two changes its
message or moves to a different test name, investigate — you may have perturbed
the preview/radius logic.

Re-verify the baseline at any time by stashing your work:

```bash
git stash -u && go test -race ./internal/timeline/... ; git stash pop
```

## Scope reminder

Both features are **read-only**. The upstream README's `wontfix` list forbids
scheduling, bulk posting, scraping, automated engagement, and mass-posting —
nothing being added here touches those. SECURITY.md pins a host allowlist; all
new requests must go to `x.com` only.
