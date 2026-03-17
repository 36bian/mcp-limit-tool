# MCP Limit Tool

A proxy server for MCP (Model Context Protocol) tools with quota management and tool filtering capabilities.

## Features

- **Quota Management**: Set hourly, daily, weekly or monthly usage quotas per MCP tool
- **Tool Filtering**: Allowlist specific tools that clients can access
- **Hot Reload**: Dynamically update configurations without restarting the server
- **Usage Tracking**: Track and persist tool usage across restarts
- **Multiple MCP Servers**: Manage multiple MCP tool backends through a single proxy

## Quick Start

### 1. Clone the Repository

```bash
git clone https://github.com/36bian/mcp-limit-tool.git
cd mcp-limit-tool
```

### 2. Configure

Edit `config/config.json` to configure your MCP server connections and quotas:

```json
{
  "app_name": {
    "command": "npx",
    "args": ["-y", "your-mcp-server"],
    "env": {
      "API_KEY": "your-api-key"
    },
    "allowed_tools": ["tool_name_1", "tool_name_2"],
    "rate_limits": {
      "hourly": {"total": 100, "duration": "1h"},
      "daily": {"total": 500, "duration": "24h"},
      "weekly": {"total": 3000, "duration": "7d"},
      "monthly": {"total": 10000, "duration": "30d"}
    }
  },
  "second_app_name": {}
}
```

### 3. Build & Run

```bash
go build -o mcp-limit-tool
./mcp-limit-tool
```

### 4. Configure IDE (VS Code / Cursor / Trae)

Add the following to your IDE's MCP configuration. The `AppName` must match the app name defined in `config.json`:

```json
{
  "mcpServers": {
    "github": {
      "command": "/path/to/mcp-limit-tool",
      "env": {
        "AppName": "github"
      }
    }
  }
}
```

> **Note**: The `AppName` env variable must match the key name in `config.json` (e.g., `"github"`, `"exa"`, `"jina"`).

## Hot Reload

The tool supports hot reloading for configuration changes:

- **Add/Remove Apps**: Modify `config.json` to add new MCP servers or remove existing ones
- **Update Quotas**: Change quota configurations in `config.json` - changes take effect immediately
- **Reset Usage**: Edit `usage.json` to manually reset usage counters

  Automatically generated file that tracks current usage. Can be manually edited to reset or adjust usage counts.

  ```json
  {
    "your_app_name": {
      "hourly": {"usage": 3, "reset_at": "2026-03-18T16:00:00"}
    }
  }
  ```

The server watches for file changes automatically - no restart required.


## Requirements

- Go 1.24+
- Node.js (for npx-based MCP servers)

## License

MIT
