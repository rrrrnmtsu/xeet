#!/bin/bash
# Runs `xeet mcp` on a headless host as the dedicated xeetmcp user, with its
# own home (config) and the private secrets file instead of an OS keyring.
# The allowlist is fixed here so no caller can widen it.
#
#   /opt/xeet-mcp/xeet-mcp.sh serve
#   /opt/xeet-mcp/xeet-mcp.sh tools
#   /opt/xeet-mcp/xeet-mcp.sh call get_x_session_health
set -eu
BASE=/opt/xeet-mcp
exec runuser -u xeetmcp -- env \
  HOME="$BASE/home" \
  XEET_MCP_SECRETS_FILE="$BASE/home/secrets.yaml" \
  XEET_MCP_ALLOWED_ACCOUNTS="${XEET_MCP_ALLOWED_ACCOUNTS:-1560376068}" \
  "$BASE/bin/xeet" mcp "$@"
