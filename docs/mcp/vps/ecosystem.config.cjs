module.exports = {
  apps: [
    {
      name: 'xeet-mcp-tunnel',
      script: '/opt/xeet-mcp/start.sh',
      cwd: '/opt/xeet-mcp',
      interpreter: 'bash',
      autorestart: true,
      restart_delay: 5000,
      max_restarts: 20,
      env: { NODE_ENV: 'production' },
    },
  ],
};
