# Reference

[Back to Quick Start](../README.md#quick-start)

## Architecture

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

## Data Model

### Node

```
{
  "node_id": "uuid-v4",
  "type": "finding | concept | blocker | question | decision | artifact | entity | event | message",
  "summary": "one-liner",
  "description": "full markdown body",
  "tags": ["redis", "performance"],
  "confidence": 0.92
}
```

`confidence` is optional. An explicit value of `0` is preserved; when omitted, it is absent from read responses.

`tags` is optional and applies to nodes only. A node may have up to 16 unique exact tags. Each tag is a lowercase ASCII slug of 1–32 characters: letters and digits separated by single hyphens. Tags are not normalized, namespaced, wildcarded, or interpreted as key/value fields.

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
| `message`  | An inter-session message (see [Messaging](using-the-graph.md#messaging)) |

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
{"seq":1,"type":"node","ts":"2026-06-18T12:00:00Z","data":{"node_id":"550e8400-e29b-41d4-a716-446655440000","type":"finding","summary":"Redis cache hit rate dropped to 40% after v2.3 deploy","description":"After deploying v2.3, Redis cache hit rate fell from 85% to 40%. Root cause: key prefix change in the new config loader.","confidence":0.92,"tags":["redis","performance"]}}
{"seq":2,"type":"edge","ts":"2026-06-18T12:00:01Z","data":{"edge_id":"660e8400-e29b-41d4-a716-446655440001","type":"causes","from_node_id":"550e8400-e29b-41d4-a716-446655440000","to_node_id":"770e8400-e29b-41d4-a716-446655440002"}}
```

- `seq` — monotonically increasing sequence number (the cursor)
- `ts` — wall-clock time of append (RFC 3339)
- Records are **never modified or deleted** — use a `deprecates` edge instead

## MCP Tools

| Tool                      | Signature                                                   | Returns                                                                          |
| ------------------------- | ----------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `list_topics`             | `()`                                                        | `[]string`                                                                       |
| `get_topic`               | `(topic)`                                                   | `{last_cursor, node_count, edge_count}` or `{archived: true}`                       |
| `read_nodes_since_cursor` | `(topic, cursor?, types?, sort?, limit?)`                           | `[]{node_id, type, summary, seq}`                                                |
| `read_nodes_since_date`   | `(topic, date, types?, sort?, limit?)`                              | `[]{node_id, type, summary, seq}`                                                |
| `read_hub_nodes`          | `(topic, limit?, types?)`                                   | `[]{node_id, type, summary, seq, neighbor_count, confidence?}`                   |
| `search_by_tags`          | `(topic, tags, match?, types?, limit?)`                     | `[]{node_id, type, summary, seq, tags}`                                          |
| `list_tags`               | `(topic, limit?)`                                           | `[]{tag, node_count}`                                                            |
| `read_node`               | `(topic, node_id)`                                          | full node + all edges (in & out)                                                 |
| `neighbors`               | `(topic, node_id, depth?, direction?, edge_types?, limit?)` | `{anchor, neighbors: []{node_id, type, summary, seq, hop, via_edge}, truncated}` |
| `write`                   | `(topic, nodes[], edges[])`                                 | `{cursor}`                                                                       |

`list_topics` includes both active and archived names. `get_topic` returns only `{"archived":true}` for an archived topic. Graph reads and writes require an active topic. Reads of missing topics return an error. A write can create a new topic, including under a name freed by renaming.

Both cursor and date reads default to a limit of 100 nodes and accept limits from 1 to 1000.

`cursor` defaults to `0`; pass `-1` to return the newest nodes first (respecting `limit`, regardless of `sort`). `date` accepts an RFC3339 timestamp or a plain `YYYY-MM-DD`. `sort` defaults to `"ascending"`; pass `"descending"` for newest-first with an ordinary cursor. `types` defaults to `["finding"]` when omitted; pass `types:["*"]` for every node type, or a specific list to narrow further.

`neighbors` does a BFS traversal from `node_id` out to `depth` hops (1–3, default 1), following `direction` (default `both`) and filtering by `edge_types` (default `["*"]`). Returns summary-level neighbors capped at `limit` (default 50, max 200); `truncated` is `true` if the reachable set exceeded `limit`.

`read_hub_nodes` returns the highest-connectivity nodes for query-independent cold starts (`limit` default 10, max 50; `types` default `["*"]`). Connectivity is the number of distinct neighbors after treating edges as undirected, collapsing parallel/opposite edges, and ignoring self-loops. `types` filters result candidates only; neighbors of every type still contribute to connectivity. Zero-neighbor nodes are omitted, so an edgeless topic returns `[]`. Results are ordered by `neighbor_count` descending, then `seq` descending, then `node_id` ascending. Optional `confidence` is preserved as output metadata but never affects ranking.

`search_by_tags` performs exact tag lookup (`match` defaults to `"all"` for AND; use `"any"` for OR). It returns summary-level nodes newest-first, defaults to all node types, and has a default limit of 50 (max 200). `list_tags` returns tags with their node counts, ordered by count descending then tag ascending (default 100, max 1000). Use returned nodes as anchors for `neighbors` and `read_node`.

The in-memory inverted index maps each tag to an append-ordered node posting list. Building it uses `O(total tag assignments)` time and space; appending a node uses `O(tags on the node)`. AND lookup walks the smallest posting list and checks the remaining requested tags. OR lookup merges the requested lists newest-first. Tag listing uses bounded top-K selection over distinct tags.

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
| `BenchmarkNodesSinceRareTypeFilter` | `read_nodes_since_cursor` filtered to a single type, with 1 matching node buried behind 50,000 nodes of another type | 57.5 ns | 64 B      | 1         |
| `BenchmarkNodesSinceWildcard`       | `read_nodes_since_cursor` with no type filter, over 50,000 nodes                                                     | 348 ns  | 581 B     | 1         |
| `BenchmarkNodesSinceMultiType`      | `read_nodes_since_cursor` merging 3 types, 10,000 nodes each                                                         | 7.48 µs | 16.0 KB   | 9         |
| `BenchmarkGetNode`                  | `read_node` on a node with 2 edges, in a 10,000-node graph                                                    | 112 ns  | 48 B      | 2         |
| `BenchmarkNeighborsChain`           | `neighbors` — depth 2, both directions, mid-chain anchor in a 10,000-node chain                               | 1.68 µs | 6.8 KB    | 11        |
| `BenchmarkNeighborsHub`             | `neighbors` — depth 1 from a hub node with 10,000 outgoing edges, limit 50                                    | 10.7 µs | 24.9 KB   | 23        |
| `BenchmarkHubNodesSparse`           | `read_hub_nodes` — top 10 from a 10,000-node, 9,999-edge chain                                               | 2.76 ms | 3.68 MB   | 257       |
| `BenchmarkSearchByTagsSelective`    | `search_by_tags` AND query over 10,000 nodes using a 10-node rare-tag posting list                            | 1.48 µs | 3.0 KB    | 15        |
| `BenchmarkSearchByTagsCommon`       | `search_by_tags` lookup of a common tag over 10,000 nodes, limited to 50 results                              | 7.07 µs | 12.9 KB   | 57        |
| `BenchmarkSearchByTagsUnion`        | `search_by_tags` OR query over three 2,500-node posting lists, limited to 50 results                          | 11.2 µs | 16.2 KB   | 65        |
| `BenchmarkTagIndexBuild`            | rebuild node/type/tag indexes for 10,000 nodes with two tag assignments each                                  | 2.75 ms | 9.43 MB   | 938       |
| `BenchmarkWriteBatch`               | `write` — single-node batch                                                                                   | 7.34 µs | 1.6 KB    | 11        |
| `BenchmarkWriteBatchLarge`          | `write` — 50-node batch                                                                                       | 83.9 µs | 68.2 KB   | 362       |
| `BenchmarkWriteParallel`            | `write` — single-node batches from concurrent callers                                                         | 7.48 µs | 1.2 KB    | 10        |
| `BenchmarkSnapshot`                 | full graph snapshot (backs `/api/graph`), 10,000 nodes + 5,000 edges                                          | 138 µs  | 121 KB    | 3         |
| `BenchmarkTopicLoad`                | cold start: opening a topic backed by an existing 20,000-node WAL file                                        | 55.9 ms | 24.0 MB   | 320,230   |

### Protocol layer (`internal/mcp`) — full JSON-RPC round trip

| Benchmark                        | Scenario                                                                          | Time/op | Memory/op | Allocs/op |
| --------------------------------- | ------------------------------------------------------------------------------------ | ------- | --------- | --------- |
| `BenchmarkWriteHandler`          | `write` tool call — JSON args in, `Write`, JSON result out                        | 7.91 µs | 2.4 KB    | 31        |
| `BenchmarkReadNodesSinceCursorHandler` | `read_nodes_since_cursor` tool call, default filter, over 1,000 seeded nodes       | 17.7 µs | 18.3 KB   | 11        |
| `BenchmarkReadHubNodesHandler`     | `read_hub_nodes` tool call, top 10 from a 1,000-node, 999-edge chain            | 268 µs  | 437 KB    | 46        |
| `BenchmarkNeighborsHandler`      | `neighbors` tool call — depth 2, limit 50, mid-chain anchor in a 1,000-node chain | 4.30 µs | 8.5 KB    | 22        |
| `BenchmarkSearchByTagsHandler`    | `search_by_tags` tool call — exact common-tag lookup over 1,000 seeded nodes      | 15.1 µs | 21.4 KB   | 69        |

Reproduce locally:

```
go test ./internal/graph/... ./internal/mcp/... -bench=. -run=^$ -benchmem
```
