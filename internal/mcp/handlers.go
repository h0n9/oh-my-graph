package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

type handlerFunc func(params json.RawMessage) (any, *RPCError)

const maxNodesSinceLimit = 1000

func callToolResult(text string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: text}}}
}

func listTopicsHandler(mgr *graph.Manager) handlerFunc {
	return func(_ json.RawMessage) (any, *RPCError) {
		topics := mgr.Topics()
		if topics == nil {
			topics = []string{}
		}
		data, _ := json.Marshal(topics)
		return callToolResult(string(data)), nil
	}
}

func getTopicHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic string `json:"topic"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic required"}
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		lastCursor, nodeCount, edgeCount := g.Stats()
		result := map[string]any{
			"last_cursor": lastCursor,
			"node_count":  nodeCount,
			"edge_count":  edgeCount,
		}
		data, _ := json.Marshal(result)
		return callToolResult(string(data)), nil
	}
}

func readNodesSinceHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic  string `json:"topic"`
			Cursor *int64 `json:"cursor"`
			Limit  *int   `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic required"}
		}

		cursor := int64(0)
		if p.Cursor != nil {
			cursor = *p.Cursor
		}

		limit := 100
		if p.Limit != nil {
			if *p.Limit <= 0 || *p.Limit > maxNodesSinceLimit {
				return nil, &RPCError{Code: -32602, Message: "invalid params: limit must be between 1 and 1000"}
			}
			limit = *p.Limit
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		summaries := g.NodesSince(cursor, limit)
		if summaries == nil {
			summaries = []graph.NodeSummary{}
		}
		data, _ := json.Marshal(summaries)
		return callToolResult(string(data)), nil
	}
}

func readNodeHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic  string `json:"topic"`
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" || p.NodeID == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic and node_id required"}
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		nwe, ok := g.GetNode(p.NodeID)
		if !ok {
			return nil, &RPCError{Code: -32602, Message: fmt.Sprintf("node %s not found", p.NodeID)}
		}

		data, _ := json.Marshal(nwe)
		return callToolResult(string(data)), nil
	}
}

func writeHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic string        `json:"topic"`
			Nodes []*graph.Node `json:"nodes"`
			Edges []*graph.Edge `json:"edges"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic required"}
		}
		if p.Nodes == nil {
			p.Nodes = []*graph.Node{}
		}
		if p.Edges == nil {
			p.Edges = []*graph.Edge{}
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		cursor, err := g.Write(p.Nodes, p.Edges)
		if err != nil {
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}

		data, _ := json.Marshal(map[string]any{"cursor": cursor})
		return callToolResult(string(data)), nil
	}
}
