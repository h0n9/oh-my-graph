package mcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

type handlerFunc func(params json.RawMessage) (any, *RPCError)

const maxNodesSinceLimit = 1000
const maxNeighborsLimit = 200
const maxHubNodesLimit = 50

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

// parseNodesSinceLimit validates the shared limit param used by both
// read_nodes_since_cursor and read_nodes_since_date, defaulting to 100.
func parseNodesSinceLimit(p *int) (int, *RPCError) {
	if p == nil {
		return 100, nil
	}
	if *p <= 0 || *p > maxNodesSinceLimit {
		return 0, &RPCError{Code: -32602, Message: "invalid params: limit must be between 1 and 1000"}
	}
	return *p, nil
}

// parseNodesSinceTypes validates the shared types param used by both
// read_nodes_since_cursor and read_nodes_since_date, defaulting to
// []NodeType{finding} when omitted, or no filtering (nil) when types
// contains "*".
func parseNodesSinceTypes(p *[]string) ([]graph.NodeType, *RPCError) {
	if p == nil {
		return []graph.NodeType{graph.NodeTypeFinding}, nil
	}
	for _, t := range *p {
		if t == "*" {
			return nil, nil
		}
	}
	types := make([]graph.NodeType, 0, len(*p))
	for _, t := range *p {
		nt := graph.NodeType(t)
		if !graph.IsValidNodeType(nt) {
			return nil, &RPCError{Code: -32602, Message: fmt.Sprintf("invalid params: unknown node type %q", t)}
		}
		types = append(types, nt)
	}
	return types, nil
}

// parseSort validates the shared sort param used by both
// read_nodes_since_cursor and read_nodes_since_date, defaulting to
// ascending (descending=false) when omitted.
func parseSort(p *string) (descending bool, rpcErr *RPCError) {
	if p == nil {
		return false, nil
	}
	switch *p {
	case "ascending":
		return false, nil
	case "descending":
		return true, nil
	default:
		return false, &RPCError{Code: -32602, Message: fmt.Sprintf("invalid params: sort must be \"ascending\" or \"descending\", got %q", *p)}
	}
}

// parseDate accepts either an RFC3339 timestamp or a plain YYYY-MM-DD date
// (treated as UTC midnight), the two formats read_nodes_since_date allows.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized date format %q: want RFC3339 or YYYY-MM-DD", s)
}

func readNodesSinceCursorHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic  string    `json:"topic"`
			Cursor *int64    `json:"cursor"`
			Limit  *int      `json:"limit"`
			Types  *[]string `json:"types"`
			Sort   *string   `json:"sort"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic required"}
		}

		limit, rpcErr := parseNodesSinceLimit(p.Limit)
		if rpcErr != nil {
			return nil, rpcErr
		}
		types, rpcErr := parseNodesSinceTypes(p.Types)
		if rpcErr != nil {
			return nil, rpcErr
		}
		descending, rpcErr := parseSort(p.Sort)
		if rpcErr != nil {
			return nil, rpcErr
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}

		cursor := int64(0)
		if p.Cursor != nil {
			cursor = *p.Cursor
			if cursor == -1 {
				// -1 is the cold-start sentinel for reading the latest nodes.
				// NodesSince uses cursor as an exclusive lower bound, so use the
				// beginning boundary and traverse the matching range newest-first.
				cursor = 0
				descending = true
			}
		}

		summaries := g.NodesSince(cursor, limit, types, descending)
		if summaries == nil {
			summaries = []graph.NodeSummary{}
		}
		data, _ := json.Marshal(summaries)
		return callToolResult(string(data)), nil
	}
}

func readNodesSinceDateHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic string    `json:"topic"`
			Date  string    `json:"date"`
			Limit *int      `json:"limit"`
			Types *[]string `json:"types"`
			Sort  *string   `json:"sort"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" || p.Date == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic and date required"}
		}

		date, err := parseDate(p.Date)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}

		limit, rpcErr := parseNodesSinceLimit(p.Limit)
		if rpcErr != nil {
			return nil, rpcErr
		}
		types, rpcErr := parseNodesSinceTypes(p.Types)
		if rpcErr != nil {
			return nil, rpcErr
		}
		descending, rpcErr := parseSort(p.Sort)
		if rpcErr != nil {
			return nil, rpcErr
		}

		g, err2 := mgr.Topic(p.Topic)
		if err2 != nil {
			return nil, &RPCError{Code: -32602, Message: err2.Error()}
		}

		summaries := g.NodesSinceDate(date, limit, types, descending)
		if summaries == nil {
			summaries = []graph.NodeSummary{}
		}
		data, _ := json.Marshal(summaries)
		return callToolResult(string(data)), nil
	}
}

func parseHubTypes(p *[]string) ([]graph.NodeType, *RPCError) {
	if p == nil {
		return nil, nil
	}
	if len(*p) == 0 {
		return nil, &RPCError{Code: -32602, Message: "invalid params: types must contain at least one item"}
	}

	hasWildcard := false
	seen := make(map[graph.NodeType]struct{}, len(*p))
	types := make([]graph.NodeType, 0, len(*p))
	for _, value := range *p {
		if value == "*" {
			hasWildcard = true
			continue
		}
		t := graph.NodeType(value)
		if !graph.IsValidNodeType(t) {
			return nil, &RPCError{Code: -32602, Message: fmt.Sprintf("invalid params: unknown node type %q", value)}
		}
		if _, exists := seen[t]; exists {
			continue
		}
		seen[t] = struct{}{}
		types = append(types, t)
	}
	if hasWildcard {
		if len(*p) != 1 {
			return nil, &RPCError{Code: -32602, Message: "invalid params: wildcard type cannot be combined with named types"}
		}
		return nil, nil
	}
	return types, nil
}

func readHubNodesHandler(mgr *graph.Manager) handlerFunc {
	return func(params json.RawMessage) (any, *RPCError) {
		var p struct {
			Topic string    `json:"topic"`
			Limit *int      `json:"limit"`
			Types *[]string `json:"types"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Topic == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params: topic required"}
		}

		limit := 10
		if p.Limit != nil {
			if *p.Limit < 1 || *p.Limit > maxHubNodesLimit {
				return nil, &RPCError{Code: -32602, Message: "invalid params: limit must be between 1 and 50"}
			}
			limit = *p.Limit
		}
		types, rpcErr := parseHubTypes(p.Types)
		if rpcErr != nil {
			return nil, rpcErr
		}

		g, err := mgr.Topic(p.Topic)
		if err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}
		hubs := g.HubNodes(limit, types)
		data, _ := json.Marshal(hubs)
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
