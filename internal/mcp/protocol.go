package mcp

import "encoding/json"

// JSON-RPC 2.0

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP protocol types

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ToolsCapability struct{}

type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
	Capabilities    Capabilities `json:"capabilities"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type CallToolResult struct {
	Content []Content `json:"content"`
}

// Tool input schemas (JSON Schema)

const listTopicsSchema = `{
  "type": "object",
  "properties": {}
}`

const getTopicSchema = `{
  "type": "object",
  "properties": {
    "topic": { "type": "string", "description": "Topic name" }
  },
  "required": ["topic"]
}`

const readNodesSinceSchema = `{
  "type": "object",
  "properties": {
    "topic":  { "type": "string", "description": "Topic name" },
    "cursor": { "type": "integer", "description": "Sequence number to read after (default: 0)", "default": 0 },
    "limit":  { "type": "integer", "description": "Maximum number of nodes to return (default: 100; must be between 1 and 1000)", "default": 100, "minimum": 1, "maximum": 1000 },
    "types":  {
      "type": "array",
      "items": { "type": "string", "enum": ["*","finding","concept","blocker","question","decision","artifact","entity","event","message"] },
      "default": ["finding"],
      "description": "Node types to include. Defaults to [\"finding\"] when omitted. Pass [\"*\"] to disable filtering and return every type."
    }
  },
  "required": ["topic"]
}`

const readNodeSchema = `{
  "type": "object",
  "properties": {
    "topic":   { "type": "string", "description": "Topic name" },
    "node_id": { "type": "string", "description": "Node ID" }
  },
  "required": ["topic", "node_id"]
}`

const neighborsSchema = `{
  "type": "object",
  "properties": {
    "topic":       { "type": "string", "description": "Topic name" },
    "node_id":     { "type": "string", "description": "Anchor node ID to traverse from" },
    "depth":       { "type": "integer", "description": "Max hops to traverse (default: 1)", "default": 1, "minimum": 1, "maximum": 3 },
    "direction":   { "type": "string", "description": "Edge direction to follow (default: both)", "enum": ["outgoing", "incoming", "both"], "default": "both" },
    "edge_types":  {
      "type": "array",
      "items": { "type": "string", "enum": ["*","resolves","produces","blocks","causes","supports","contradicts","depends_on","part_of","references","replies_to","deprecates"] },
      "default": ["*"],
      "description": "Edge types to include. Defaults to [\"*\"] (all types). Distinct from read_nodes_since's node-type filter."
    },
    "limit": { "type": "integer", "description": "Maximum number of neighbors to return (default: 50; must be between 1 and 200)", "default": 50, "minimum": 1, "maximum": 200 }
  },
  "required": ["topic", "node_id"]
}`

const writeSchema = `{
  "type": "object",
  "properties": {
    "topic": { "type": "string", "description": "Topic name" },
    "nodes": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "node_id":     { "type": "string" },
          "type":        { "type": "string", "enum": ["finding","concept","blocker","question","decision","artifact","entity","event","message"] },
          "summary":     { "type": "string" },
          "description": { "type": "string" },
          "confidence":  { "type": "number", "minimum": 0, "maximum": 1 }
        },
        "required": ["node_id", "type", "summary"]
      }
    },
    "edges": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "edge_id":      { "type": "string" },
          "type":         { "type": "string", "enum": ["resolves","produces","blocks","causes","supports","contradicts","depends_on","part_of","references","replies_to","deprecates"] },
          "from_node_id": { "type": "string" },
          "to_node_id":   { "type": "string" }
        },
        "required": ["edge_id", "type", "from_node_id", "to_node_id"]
      }
    }
  },
  "required": ["topic"]
}`
