package mcp

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

type Server struct {
	manager  *graph.Manager
	toolDefs []Tool
	handlers map[string]handlerFunc
}

func NewServer(mgr *graph.Manager) *Server {
	s := &Server{
		manager:  mgr,
		handlers: make(map[string]handlerFunc),
	}
	s.registerTools()
	return s
}

func (s *Server) registerTools() {
	register := func(name, desc, schema string, h handlerFunc) {
		s.toolDefs = append(s.toolDefs, Tool{
			Name:        name,
			Description: desc,
			InputSchema: json.RawMessage(schema),
		})
		s.handlers[name] = h
	}

	register("list_topics", "List all available topics", listTopicsSchema, listTopicsHandler(s.manager))
	register("get_topic", "Get topic metadata including the last cursor, node count, and edge count", getTopicSchema, getTopicHandler(s.manager))
	register("read_nodes_since", "Read node summaries added after the given cursor (defaults to 0); use limit for pagination (default: 100); returns finding nodes only by default — pass types to narrow further or types:[\"*\"] for every type", readNodesSinceSchema, readNodesSinceHandler(s.manager))
	register("read_node", "Get a node's full data along with all its edges (incoming and outgoing)", readNodeSchema, readNodeHandler(s.manager))
	register("neighbors", "Get a node's neighbors via BFS traversal (default depth 1, both directions, all edge types); returns summary-level nodes with hop distance and connecting edge — use for graph-local relevance expansion before read_node", neighborsSchema, neighborsHandler(s.manager))
	register("write", "Write nodes and/or edges to a topic; creates the topic if it does not exist", writeSchema, writeHandler(s.manager))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nil, -32700, "parse error")
		return
	}

	if req.JSONRPC != "2.0" {
		writeError(w, req.ID, -32600, "invalid request: jsonrpc must be \"2.0\"")
		return
	}

	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, &InitializeResult{
			ProtocolVersion: "2025-03-26",
			ServerInfo:      ServerInfo{Name: "oh-my-graph", Version: "0.1.0"},
			Capabilities:    Capabilities{Tools: &ToolsCapability{}},
		})

	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)

	case "ping":
		writeResult(w, req.ID, map[string]any{})

	case "tools/list":
		writeResult(w, req.ID, &ToolsListResult{Tools: s.toolDefs})

	case "tools/call":
		s.handleToolCall(w, req)

	default:
		if req.ID == nil {
			// unknown notification — no response
			w.WriteHeader(http.StatusNoContent)
		} else {
			writeError(w, req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *Server) handleToolCall(w http.ResponseWriter, req Request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		writeError(w, req.ID, -32602, "invalid params: name required")
		return
	}

	h, ok := s.handlers[p.Name]
	if !ok {
		writeError(w, req.ID, -32602, "unknown tool: "+p.Name)
		return
	}

	if p.Arguments == nil {
		p.Arguments = json.RawMessage("{}")
	}

	result, rpcErr := h(p.Arguments)
	if rpcErr != nil {
		writeError(w, req.ID, rpcErr.Code, rpcErr.Message)
		return
	}

	writeResult(w, req.ID, result)
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resp := Response{JSONRPC: "2.0", ID: id, Result: result}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("oh-my-graph: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("oh-my-graph: encode error response: %v", err)
	}
}
