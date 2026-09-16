#!/bin/bash
# PM2 entry point: OpenAI Secure MCP Tunnel client in front of `xeet mcp serve`.
# .env holds CONTROL_PLANE_API_KEY and CONTROL_PLANE_TUNNEL_ID (root:root, 0600);
# nothing else reads it and PM2 never sees the values.
set -a
# shellcheck disable=SC1091
source /opt/xeet-mcp/.env
set +a
cd /opt/xeet-mcp
exec ./.runtime/tunnel-client run \
  --profile xeet-mcp \
  --profile-dir /opt/xeet-mcp/tunnel-profile \
  --mcp.max-concurrent-requests 1 \
  --health.url-file /opt/xeet-mcp/tunnel-profile/health-url
