package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

type handlerFunc func(params json.RawMessage) (any, *RPCError)

const maxNodesSinceLimit = 1000
const maxNeighborsLimit = 200

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
			Topic  string    `json:"topic"`
			Cursor *int64    `json:"cursor"`
			Limit  *int      `json:"limit"`
			Types  *[]string `json:"types"`
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

		var types []graph.NodeType
		switch {
		case p.Types == nil:
			types = []graph.NodeType{graph.NodeTypeFinding}
		default:
			wildcard := false
			for _, t := range *p.Types {
				if t == "*" {
					wildcard = true
					break
				}
			}
			if !wildcard {
				types = make([]graph.NodeType, 0, len(*p.Types))
				for _, t := range *p.Types {
					nt := graph.NodeType(t)
					if !graph.IsValidNodeType(nt) {
						return nil, &RPCError{Code: -32602, Message: fmt.Sprintf("invalid params: unknown node type %q", t)}
					}
					types = append(types, nt)
				}
			}
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		summaries := g.NodesSince(cursor, limit, types)
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

func neighborsHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic     string    `json:"topic"`
			NodeID    string    `json:"node_id"`
			Depth     *int      `json:"depth"`
			Direction *string   `json:"direction"`
			EdgeTypes *[]string `json:"edge_types"`
			Limit     *int      `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" || p.NodeID == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic and node_id required"}
		}

		depth := 1
		if p.Depth != nil {
			if *p.Depth < 1 || *p.Depth > 3 {
				return nil, &RPCError{Code: -32602, Message: "invalid params: depth must be between 1 and 3"}
			}
			depth = *p.Depth
		}

		direction := "both"
		if p.Direction != nil {
			if *p.Direction != "outgoing" && *p.Direction != "incoming" && *p.Direction != "both" {
				return nil, &RPCError{Code: -32602, Message: "invalid params: direction must be one of outgoing, incoming, both"}
			}
			direction = *p.Direction
		}

		var edgeTypes []graph.EdgeType
		if p.EdgeTypes != nil {
			wildcard := false
			for _, t := range *p.EdgeTypes {
				if t == "*" {
					wildcard = true
					break
				}
			}
			if !wildcard {
				edgeTypes = make([]graph.EdgeType, 0, len(*p.EdgeTypes))
				for _, t := range *p.EdgeTypes {
					et := graph.EdgeType(t)
					if !graph.IsValidEdgeType(et) {
						return nil, &RPCError{Code: -32602, Message: fmt.Sprintf("invalid params: unknown edge type %q", t)}
					}
					edgeTypes = append(edgeTypes, et)
				}
			}
		}

		limit := 50
		if p.Limit != nil {
			if *p.Limit <= 0 || *p.Limit > maxNeighborsLimit {
				return nil, &RPCError{Code: -32602, Message: "invalid params: limit must be between 1 and 200"}
			}
			limit = *p.Limit
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		result, ok := g.Neighbors(p.NodeID, depth, direction, edgeTypes, limit)
		if !ok {
			return nil, &RPCError{Code: -32602, Message: fmt.Sprintf("node %s not found", p.NodeID)}
		}

		data, _ := json.Marshal(result)
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
