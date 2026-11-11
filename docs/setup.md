# Setup

[Back to README](../README.md)

## Connecting AI Clients

Point your MCP client at `http://localhost:7780/mcp` (Streamable HTTP transport, JSON-RPC 2.0) for a local install, or at your remote server's public URL for a remote one.

### Claude Desktop

For a local server, use [`mcp-remote`](https://github.com/geelen/mcp-remote) to bridge Claude Desktop’s local stdio configuration to HTTP. Install Node.js so `npx` is available.

Add the following to your Claude Desktop config file (macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`; Windows: `%APPDATA%\Claude\claude_desktop_config.json`):

```
{
  "mcpServers": {
    "oh-my-graph": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "http://localhost:7780/mcp"
      ]
    }
  }
}
```

Restart Claude Desktop and check that the `oh-my-graph` tools are available.

### Claude Code

Claude Code natively supports Streamable HTTP MCP — no bridge required.

**Via CLI** (global scope, so the server is available from every project):

```
claude mcp add oh-my-graph --transport http --scope user http://localhost:7780/mcp
```

For project scope, run the command with `--scope project` from the project directory.

**Tip:** Add the following to your `~/.claude/CLAUDE.md` so Claude automatically loads graph context at the start of every session:

```
## oh-my-graph Knowledge Graph

At the start of every session, connect to the `oh-my-graph` MCP server:

1. Call `list_topics` to discover topic names.
2. Pick a topic based on the working directory, project name, or the user's first message. If it exists, check `get_topic` and skip it if it returns `archived: true`.
3. For exact categorical discovery, call `list_tags(<topic>)`, then `search_by_tags(<topic>, tags: [...])`. With no specific retrieval intent, call `read_hub_nodes(<topic>)` for a compact map of the topic's most-connected nodes.
4. Call `neighbors(<topic>, node_id, depth: 2)` on selected anchors to pull in graph-local context, then use `read_node` only for the final nodes that need full details. Use `read_nodes_since_date` for time-scoped context or `read_nodes_since_cursor` when resuming from a known checkpoint.

During the session, call `write` frequently to persist findings, decisions, and artifacts. Link related nodes with edges to preserve reasoning chains.
```

### Codex

```sh
codex mcp add oh-my-graph --url http://localhost:7780/mcp
```

Run `codex mcp list` to confirm registration, then start a new Codex session.

## Run the server

The install script accepts `INSTALL_DIR` for a custom install directory and `VERSION=vX.Y.Z` to pin a release.

Run the binary to choose a port or data directory.

```
oh-my-graph                  # listens on :7780, data at ~/.oh-my-graph
oh-my-graph --port 8080      # custom port
oh-my-graph --data /var/omg  # custom data directory
```

The server loads each topic graph into memory on first access and flushes writes to disk asynchronously. Multiple agents may connect concurrently.

## Security and Authentication

The server listens on all network interfaces by default. Restrict access with a firewall for local use. If other machines can reach the server, enable authentication and use HTTPS. This includes access through a reverse proxy, tunnel, or port forward.

If you followed Quick Start, stop the running server first. For Homebrew, run `brew services stop h0n9/devops/oh-my-graph`. For the installer, press Ctrl-C in the server terminal.

Replace both example values and start the server with `--auth`. Leave this terminal running. If the installer directory is missing from your PATH, use `"$HOME/.local/bin/oh-my-graph" --auth` as the last command.

```sh
export OMG_ISSUER='https://your-public-base-url'
export OMG_OWNER_PASSPHRASE='replace-with-your-secret'
oh-my-graph --auth
```

This turns on a full OAuth 2.1 Authorization Code + PKCE flow with Dynamic Client Registration — MCP clients self-register and your browser prompts once for the passphrase; bearer tokens on `/mcp` and `/omg-mcp` authorize every call after that. The web UI, including topic management, uses a separate session cookie issued by the in-app `/login` page after you enter the same passphrase.

Use a reverse proxy to terminate HTTPS and limit inbound access with firewall rules where possible. See [Deploy on Sprites](#deploy-on-sprites) for a worked example.

## Deploy on Sprites

Set up [authentication](#security-and-authentication) before exposing your server. Keep the default private Sprite gateway while you set up the service. Choose gateway authentication for clients that can send a Sprite token, or oh-my-graph OAuth for clients that use OAuth.

1. Create the Sprite and install oh-my-graph.

   ```
   curl -fsSL https://sprites.dev/install.sh | sh
   sprite org auth
   sprite create oh-my-graph
   sprite use oh-my-graph
   ```

   ```
   sprite exec -- bash -c "export INSTALL_DIR=/home/sprite; curl -fsSL https://raw.githubusercontent.com/h0n9/oh-my-graph/main/install.sh | sh"
   ```

2. If you have existing data, stop writes to the source graph before archiving it. Upload and extract the archive before you start the service.

   ```
   tar -czf data.tar.gz -C ~/.oh-my-graph .
   sprite exec --file "data.tar.gz:/home/sprite/data.tar.gz" -- bash -c "mkdir -p /home/sprite/.oh-my-graph && tar -xzf /home/sprite/data.tar.gz -C /home/sprite/.oh-my-graph && rm /home/sprite/data.tar.gz"
   ```

3. Start the service using one of the authentication options below.

### Sprite gateway authentication

Keep the Sprite URL in its default `sprite` auth mode. Clients need a valid Sprites organization bearer token.

```
sprite exec -- sprite-env services create oh-my-graph --cmd /home/sprite/oh-my-graph --args "--port,7780,--data,/home/sprite/.oh-my-graph" --http-port 7780
```

```
curl -H "Authorization: Bearer $SPRITE_TOKEN" https://<sprite>-<org>.sprites.app/omg-mcp
```

### oh-my-graph OAuth

Replace the placeholders in the service command. Start the authenticated service before making the Sprite gateway public.

```
sprite exec -- sprite-env services create oh-my-graph --cmd /home/sprite/oh-my-graph --args "--port,7780,--data,/home/sprite/.oh-my-graph,--auth" --env "OMG_ISSUER=https://<sprite>-<org>.sprites.app,OMG_OWNER_PASSPHRASE=<your-passphrase>" --http-port 7780
sprite url update --auth public -s oh-my-graph
```

Set the connector URL in your client to `https://<sprite>-<org>.sprites.app/omg-mcp`. The client registers itself and your browser prompts for the owner passphrase. Use `/omg-mcp` on Sprites because the gateway reserves `/mcp` for its own server.

## Syncing across devices

Symlinking the data directory into iCloud Drive lets you share your graph across multiple Macs and browse it on iPhone.

**Fresh install (no existing data):**

```
mkdir -p "$HOME/Library/Mobile Documents/com~apple~CloudDocs/oh-my-graph"
ln -s "$HOME/Library/Mobile Documents/com~apple~CloudDocs/oh-my-graph" ~/.oh-my-graph
brew services start h0n9/devops/oh-my-graph
```

**Existing data at `~/.oh-my-graph`** — back up first (`cp -r ~/.oh-my-graph ~/.oh-my-graph.bak`), then:

```
brew services stop h0n9/devops/oh-my-graph
mv ~/.oh-my-graph "$HOME/Library/Mobile Documents/com~apple~CloudDocs/oh-my-graph"
ln -s "$HOME/Library/Mobile Documents/com~apple~CloudDocs/oh-my-graph" ~/.oh-my-graph
brew services start h0n9/devops/oh-my-graph
```

On each additional Mac, preserve any existing local graph in a backup before linking to the shared directory. The example assumes `~/.oh-my-graph.local-backup` is available.

```
brew services stop h0n9/devops/oh-my-graph
if [ -d ~/.oh-my-graph ] && [ ! -L ~/.oh-my-graph ]; then
  mv -n ~/.oh-my-graph ~/.oh-my-graph.local-backup
fi
ln -s "$HOME/Library/Mobile Documents/com~apple~CloudDocs/oh-my-graph" ~/.oh-my-graph
brew services start h0n9/devops/oh-my-graph
```

> Make sure only one machine runs the server at a time to avoid concurrent writes to the same file.
