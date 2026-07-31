# oh-my-graph

**Use the best AI for every task, not the same AI for every task.**

`oh-my-graph` gives AI agents a shared, persistent knowledge graph — implemented as an MCP server — so different assistants can collaborate through the same continuously updated context instead of starting from zero every session. See [Why oh-my-graph](#why-oh-my-graph) for the full idea, or [a concrete example](#concrete-example-a-day-with-oh-my-graph) of it in action.

[![oh-my-graph visualization](https://github.com/h0n9/oh-my-graph/raw/main/assets/screenshot.png)](/h0n9/oh-my-graph/blob/main/assets/screenshot.png)

## Installation

### Local Machine

**macOS** — installs as a launchd service that starts automatically on login:

```
brew install h0n9/devops/oh-my-graph
brew services start h0n9/devops/oh-my-graph
```

If you'd like to share the graph across multiple Macs, see [Syncing across devices](#syncing-across-devices).

**Linux (or macOS without Homebrew)** — one-line installer, detects OS/arch and installs to `~/.local/bin` (override with `INSTALL_DIR`; pin a version with `VERSION=vX.Y.Z`):

```
curl -fsSL https://raw.githubusercontent.com/h0n9/oh-my-graph/main/install.sh | sh
```

The server runs on port **7780** by default.

### Server Deployment

Prefer not to run it on your own machine? Host `oh-my-graph` as an always-on remote MCP server on any platform that can run the Go binary and expose a port — a VPS, a cloud VM, a container host, etc. The walkthrough below uses [Sprites](https://sprites.dev) as one concrete example; the same binary and flags work anywhere.

**Example: deploying on Sprites**

```
curl -fsSL https://sprites.dev/install.sh | sh
sprite org auth
sprite create oh-my-graph
sprite use oh-my-graph
```

Install the release binary onto the sprite with the same installer used locally:

```
sprite exec -- bash -c "export INSTALL_DIR=/home/sprite; curl -fsSL https://raw.githubusercontent.com/h0n9/oh-my-graph/main/install.sh | sh"
```

Register it as a persistent [service](https://docs.sprites.dev/concepts/services/), bound to the sprite's public port so it restarts across hibernation/reboot:

```
sprite exec -- sprite-env services create oh-my-graph --cmd /home/sprite/oh-my-graph --args "--port,7780,--data,/home/sprite/.oh-my-graph" --http-port 7780
```

**Migrating existing data:** tar up your local data directory and upload it before starting the service:

```
tar -czf data.tar.gz -C ~/.oh-my-graph .
sprite exec --file "data.tar.gz:/home/sprite/data.tar.gz" -- bash -c "mkdir -p /home/sprite/.oh-my-graph && tar -xzf /home/sprite/data.tar.gz -C /home/sprite/.oh-my-graph && rm /home/sprite/data.tar.gz"
```

## Security and Authentication

Whether authentication is required depends on network exposure, not on where you host `oh-my-graph`. If the server is only ever reached via `localhost`, no auth is needed — that's the default for a local install. The moment the port is bound to a non-localhost interface, or exposed through a reverse proxy, tunnel, or port forward — self-hosted on a VPS/cloud box, or hosted on Sprites — you should enable auth.

Enable it with `--auth`, which requires two environment variables:

```
OMG_ISSUER=https://your-public-base-url
OMG_OWNER_PASSPHRASE=<a-secret-only-you-know>
```

This turns on a full OAuth 2.1 Authorization Code + PKCE flow with Dynamic Client Registration — MCP clients self-register and your browser prompts once for the passphrase; bearer tokens on `/mcp` and `/omg-mcp` authorize every call after that. The web visualization UI (`/` and `/graph`) is protected separately, gated by the same passphrase via HTTP Basic auth.

**Recommendation:** treat any non-localhost binding as a public endpoint by default and require `--auth`, paired with standard network hygiene — HTTPS termination via reverse proxy, firewall rules limiting source IPs.

See [Connecting AI Clients → Connecting to a remote server](#connecting-to-a-remote-server-sprites-example) for a worked walkthrough of this mechanism, plus a platform-specific shortcut for clients that don't need it.

## Connecting AI Clients

Point your MCP client at `http://localhost:7780/mcp` (Streamable HTTP transport, JSON-RPC 2.0) for a local install, or at your remote server's public URL for a remote one.

### Claude Desktop

Claude Desktop only supports stdio-based MCP servers. Use [`mcp-remote`](https://github.com/geelen/mcp-remote) as a bridge to the HTTP server.

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

Then restart Claude Desktop. The `oh-my-graph` tools (`list_topics`, `get_topic`, `read_nodes_since`, `read_node`, `neighbors`, `write`) will appear automatically.

### Claude Code

Claude Code natively supports Streamable HTTP MCP — no bridge required.

**Via CLI** (global scope, so the server is available from every project):

```
claude mcp add oh-my-graph --transport http --scope user http://localhost:7780/mcp
```

**Manually** — add to `~/.claude.json` (global) or `.claude/settings.json` (project):

```
{
  "mcpServers": {
    "oh-my-graph": {
      "type": "http",
      "url": "http://localhost:7780/mcp"
    }
  }
}
```

**Tip:** Add the following to your `~/.claude/CLAUDE.md` so Claude automatically loads graph context at the start of every session:

```
## oh-my-graph Knowledge Graph

At the start of every session, connect to the `oh-my-graph` MCP server:

1. Call `list_topics` to discover existing topics.
2. Infer the topic from context — working directory name, project name, or the user's first message.
3. Call `read_nodes_since(<topic>)` (cursor 0) to load existing findings before responding. This defaults to `finding` nodes only — pass `types` (e.g. `["decision"]`) or `types:["*"]` if the session needs decisions, blockers, or questions too.
4. When a node from that skim looks relevant, call `neighbors(<topic>, node_id, depth: 2)` to pull in its graph-local context before deciding whether to spend a full `read_node` call on it.

During the session, call `write` frequently to persist findings, decisions, and artifacts. Link related nodes with edges to preserve reasoning chains.
```

### Codex

Add to `~/.codex/config.yaml`:

```
mcp_servers:
  - name: oh-my-graph
    type: http
    url: http://localhost:7780/mcp
```

### Connecting to a remote server (Sprites example)

See [Security and Authentication](#security-and-authentication) for when you need this. Option B is `oh-my-graph`'s own platform-agnostic auth mechanism and works identically on any host; Option A is a shortcut specific to this platform's own gateway.

**Option A — Sprite gateway auth (default on this platform).** Leave the sprite's URL at its default `sprite` auth mode and run without `--auth`. Any client with a valid Sprites org bearer token can connect:

```
curl -H "Authorization: Bearer $SPRITE_TOKEN" https://<sprite>-<org>.sprites.app/omg-mcp
```

This doesn't work for clients with no header/static-credential field — notably ChatGPT. Use Option B for those.

**Option B — OMG OAuth (opt-in).** Run with `--auth` and set `OMG_ISSUER` (the server's public base URL) and `OMG_OWNER_PASSPHRASE` (a shared secret). This enables a full OAuth 2.1 Authorization Code + PKCE flow with Dynamic Client Registration, gated by the passphrase:

```
sprite exec -- sprite-env services create oh-my-graph --cmd /home/sprite/oh-my-graph --args "--port,7780,--data,/home/sprite/.oh-my-graph,--auth" --env "OMG_ISSUER=https://<sprite>-<org>.sprites.app,OMG_OWNER_PASSPHRASE=<your-passphrase>" --http-port 7780
sprite url update --auth public -s oh-my-graph
```

Then just add `https://<sprite>-<org>.sprites.app/omg-mcp` as the connector URL in Claude or ChatGPT — no header, no client ID or secret needed. The client self-registers via DCR and your browser prompts once for the passphrase.

> **Note:** Sprites' own gateway reserves `/mcp` on every `*.sprites.app` URL for its own control-plane server, so `oh-my-graph` also serves its MCP handler at `/omg-mcp` — use that path when connecting through a sprite's public URL.

## Overview

### Why oh-my-graph

Modern AI assistants are powerful, but every conversation is an island. The moment you switch from ChatGPT to Claude to any other assistant, the context you built disappears — ideas, decisions, and architecture choices have to be re-explained from scratch.

`oh-my-graph` solves this by separating **knowledge** from **conversations**, giving different assistants a shared, persistent layer to collaborate through instead of starting from zero every session.

Every new session — a new terminal, a new agent, a new person on the team — normally starts from zero, no matter how much a previous session figured out. Dropbox stores your files. Git stores the evolution of your project. Chat history stores a conversation. `oh-my-graph` stores the evolution of your project's *understanding* — the findings, decisions, and open questions multiple agents accumulate while working on it, so the next session picks up where the last one left off.

Concretely, that means:

- **Persist findings** across sessions
- **Share knowledge** between concurrent agents working on the same project
- **Pass messages** between sessions using `message` nodes and `replies_to` edges
- **Track reasoning** with `supports`, `contradicts`, `causes`, `deprecates` edges

### Concrete Example: A Day with `oh-my-graph`

**Scenario: Turning a walking conversation into an implemented feature**

A developer has an idea while walking outside.

**Step 1 — Capture ideas naturally with ChatGPT Voice**

While walking, the developer opens ChatGPT Voice and talks naturally: *"I think AI assistants are becoming more powerful, but the biggest problem is that my knowledge is scattered across different conversations. I want a way to keep my ideas and context available everywhere."*

Instead of writing notes manually, the developer explores the idea conversationally. ChatGPT helps by asking questions, discovering missing perspectives, structuring the idea, identifying use cases, and summarizing decisions. The important insights are stored in `oh-my-graph`:

- **Problem:** "AI conversations are isolated across platforms."
- **Insight:** "Knowledge should exist independently from any AI assistant."
- **Decision:** "`oh-my-graph` should become a shared context layer."

**Step 2 — Continue implementation with Claude Code**

Later, back at the desk, the developer opens Claude Code. It doesn't need to be told everything again — it reads the existing graph context: the original problem, previous discussions, architecture decisions, technical constraints, implementation ideas. It proposes a plan: *"Based on the graph context, we should implement a new MCP integration that allows AI clients to access shared knowledge nodes."* The developer reviews it, says "Yes, implement it," and Claude Code writes the code.

**Step 3 — Use another AI for communication and marketing**

After implementation, the developer brings in an assistant that's stronger at communication. It reads the same graph context and helps with README improvements, blog posts, product positioning, documentation, and user guides — understanding the original vision because the context was already preserved.

**The result**

The developer is no longer switching between disconnected AI conversations:

- **ChatGPT Voice** → explores ideas
- **`oh-my-graph`** → preserves knowledge
- **Claude Code** → builds the solution
- **Other AI assistants** → communicate and expand the idea

Each AI does what it does best. The user's knowledge remains continuous — *your AI assistants may change, but your knowledge stays with you.*

### Architecture

`oh-my-graph` runs as an HTTP server exposing a [Model Context Protocol (MCP)](https://modelcontextprotocol.io) interface. Multiple AI agents connect to a single server instance and share knowledge organized into **topics**.

Knowledge is stored as a graph of **nodes** (facts, concepts, questions, decisions, messages, ...) and **edges** (causal, epistemic, conversational relationships). The graph is persisted as an append-only JSONL file (`graph.jsonl`) per topic — like a write-ahead log:

```
~/.oh-my-graph/
├── life/
│   └── graph.jsonl
├── project-x/
│   └── graph.jsonl
└── comms/
    └── graph.jsonl
```

### Graph Visualization

Open **`http://localhost:7780/`** in your browser to explore the graph visually — the topic list shows node/edge counts, and `/graph?topic=<name>` renders a live interactive force-directed graph.

## Data Model

### Node

```
{
  "node_id": "uuid-v4",
  "type": "finding | concept | blocker | question | decision | artifact | entity | event | message",
  "summary": "one-liner",
  "description": "full markdown body",
  "confidence": 0.92
}
```

| Type       | Purpose                                                |
| ---------- | ------------------------------------------------------ |
| `finding`  | A discovered fact or observation                       |
| `concept`  | An abstract idea or principle                          |
| `blocker`  | Something preventing progress                          |
| `question` | An open unknown                                        |
| `decision` | A made choice with rationale                           |
| `artifact` | A produced item (file, PR, doc)                        |
| `entity`   | A real-world thing (person, system, service)           |
| `event`    | Something that happened                                |
| `message`  | An inter-session message (see [Messaging](#messaging)) |

### Edge

```
{
  "edge_id": "uuid-v4",
  "type": "resolves | produces | blocks | causes | supports | contradicts | depends_on | part_of | references | replies_to | deprecates",
  "from_node_id": "uuid-v4",
  "to_node_id": "uuid-v4"
}
```

| Type          | Meaning                       |
| ------------- | ----------------------------- |
| `resolves`    | Solution → blocker            |
| `produces`    | Process → artifact            |
| `blocks`      | Blocker → target              |
| `causes`      | Cause → effect                |
| `supports`    | Evidence → claim              |
| `contradicts` | Counter-evidence → claim      |
| `depends_on`  | A requires B                  |
| `part_of`     | A belongs to B                |
| `references`  | A cites B                     |
| `replies_to`  | Message → message (threading) |
| `deprecates`  | New node supersedes old node  |

### Storage format (`graph.jsonl`)

Each line is a WAL record:

```
{"seq":1,"type":"node","ts":"2026-06-18T12:00:00Z","data":{"node_id":"550e8400-e29b-41d4-a716-446655440000","type":"finding","summary":"Redis cache hit rate dropped to 40% after v2.3 deploy","description":"After deploying v2.3, Redis cache hit rate fell from 85% to 40%. Root cause: key prefix change in the new config loader.","confidence":0.92}}
{"seq":2,"type":"edge","ts":"2026-06-18T12:00:01Z","data":{"edge_id":"660e8400-e29b-41d4-a716-446655440001","type":"causes","from_node_id":"550e8400-e29b-41d4-a716-446655440000","to_node_id":"770e8400-e29b-41d4-a716-446655440002"}}
```

- `seq` — monotonically increasing sequence number (the cursor)
- `ts` — wall-clock time of append (RFC 3339)
- Records are **never modified or deleted** — use a `deprecates` edge instead

## MCP Tools

| Tool               | Signature                                                   | Returns                                                                          |
| ------------------ | ----------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `list_topics`      | `()`                                                        | `[]string`                                                                       |
| `get_topic`        | `(topic)`                                                   | `{last_cursor, node_count, edge_count}`                                          |
| `read_nodes_since` | `(topic, cursor?, types?)`                                  | `[]{node_id, type, summary, seq}`                                                |
| `read_node`        | `(topic, node_id)`                                          | full node + all edges (in & out)                                                 |
| `neighbors`        | `(topic, node_id, depth?, direction?, edge_types?, limit?)` | `{anchor, neighbors: []{node_id, type, summary, seq, hop, via_edge}, truncated}` |
| `write`            | `(topic, nodes[], edges[])`                                 | `{cursor}`                                                                       |

`cursor` defaults to `0`. `types` defaults to `["finding"]` when omitted; pass `types:["*"]` for every node type, or a specific list to narrow further.

`neighbors` does a BFS traversal from `node_id` out to `depth` hops (1–3, default 1), following `direction` (default `both`) and filtering by `edge_types` (default `["*"]`). Returns summary-level neighbors capped at `limit` (default 50, max 200); `truncated` is `true` if the reachable set exceeded `limit`.

## Messaging

Agents communicate asynchronously via `message` nodes in a shared topic:

1. **Session A** writes a `message` node to topic `"comms"`
2. **Session B** polls `read_nodes_since("comms", last_cursor, types:["message"])` and sees the message
3. **Session B** replies with a new `message` node + `replies_to` edge pointing back

No extra infrastructure needed — the graph is the message bus.

## Usage

Start the server:

```
oh-my-graph                  # listens on :7780, data at ~/.oh-my-graph
oh-my-graph --port 8080      # custom port
oh-my-graph --data /var/omg  # custom data directory
```

The server loads each topic graph into memory on first access and flushes writes to disk asynchronously. Multiple agents may connect concurrently.

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

On each additional Mac:

```
brew services stop h0n9/devops/oh-my-graph
rm -rf ~/.oh-my-graph
ln -s "$HOME/Library/Mobile Documents/com~apple~CloudDocs/oh-my-graph" ~/.oh-my-graph
brew services start h0n9/devops/oh-my-graph
```

> Make sure only one machine runs the server at a time to avoid concurrent writes to the same file.

## Development

```
git clone https://github.com/h0n9/oh-my-graph
cd oh-my-graph
make run    # go run — starts the server on port 7780
make build  # produces ./oh-my-graph binary
make clean  # removes the binary
```

Requires Go 1.26+. No external dependencies.

## Benchmarks

Measured on Apple M1 Pro (`go test ./internal/graph/... ./internal/mcp/... -bench=. -run=^$ -benchmem`).

### Storage layer (`internal/graph`)

| Benchmark                           | Scenario                                                                                                     | Time/op | Memory/op | Allocs/op |
| ------------------------------------ | -------------------------------------------------------------------------------------------------------------- | ------- | --------- | --------- |
| `BenchmarkNodesSinceRareTypeFilter` | `read_nodes_since` filtered to a single type, with 1 matching node buried behind 50,000 nodes of another type | 57.5 ns | 64 B      | 1         |
| `BenchmarkNodesSinceWildcard`       | `read_nodes_since` with no type filter, over 50,000 nodes                                                     | 348 ns  | 581 B     | 1         |
| `BenchmarkNodesSinceMultiType`      | `read_nodes_since` merging 3 types, 10,000 nodes each                                                         | 7.48 µs | 16.0 KB   | 9         |
| `BenchmarkGetNode`                  | `read_node` on a node with 2 edges, in a 10,000-node graph                                                    | 112 ns  | 48 B      | 2         |
| `BenchmarkNeighborsChain`           | `neighbors` — depth 2, both directions, mid-chain anchor in a 10,000-node chain                               | 1.68 µs | 6.8 KB    | 11        |
| `BenchmarkNeighborsHub`             | `neighbors` — depth 1 from a hub node with 10,000 outgoing edges, limit 50                                    | 10.7 µs | 24.9 KB   | 23        |
| `BenchmarkWriteBatch`               | `write` — single-node batch                                                                                   | 7.34 µs | 1.6 KB    | 11        |
| `BenchmarkWriteBatchLarge`          | `write` — 50-node batch                                                                                       | 83.9 µs | 68.2 KB   | 362       |
| `BenchmarkWriteParallel`            | `write` — single-node batches from concurrent callers                                                         | 7.48 µs | 1.2 KB    | 10        |
| `BenchmarkSnapshot`                 | full graph snapshot (backs `/api/graph`), 10,000 nodes + 5,000 edges                                          | 138 µs  | 121 KB    | 3         |
| `BenchmarkTopicLoad`                | cold start: opening a topic backed by an existing 20,000-node WAL file                                        | 55.9 ms | 24.0 MB   | 320,230   |

### Protocol layer (`internal/mcp`) — full JSON-RPC round trip

| Benchmark                        | Scenario                                                                          | Time/op | Memory/op | Allocs/op |
| --------------------------------- | ------------------------------------------------------------------------------------ | ------- | --------- | --------- |
| `BenchmarkWriteHandler`          | `write` tool call — JSON args in, `Write`, JSON result out                        | 7.91 µs | 2.4 KB    | 31        |
| `BenchmarkReadNodesSinceHandler` | `read_nodes_since` tool call, default filter, over 1,000 seeded nodes             | 17.7 µs | 18.3 KB   | 11        |
| `BenchmarkNeighborsHandler`      | `neighbors` tool call — depth 2, limit 50, mid-chain anchor in a 1,000-node chain | 4.30 µs | 8.5 KB    | 22        |

Reproduce locally:

```
go test ./internal/graph/... ./internal/mcp/... -bench=. -run=^$ -benchmem
```

## License

Apache 2.0 — see [LICENSE](https://github.com/h0n9/oh-my-graph/blob/main/LICENSE) for details.
