#!/bin/sh
# Launchd entry point for the ChatGPT Secure MCP Tunnel in front of `xeet mcp serve`.
# Secrets come from an env file the job owner can read, never from the plist.
set -eu
CONFIG_DIR="$HOME/.config/xeet-mcp"
set -a
. "$CONFIG_DIR/tunnel.env"   # CONTROL_PLANE_API_KEY, CONTROL_PLANE_TUNNEL_ID
set +a
exec "$HOME/.local/bin/tunnel-client" run \
  --profile xeet \
  --profile-dir "$CONFIG_DIR/tunnel-profile"
