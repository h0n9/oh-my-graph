package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

func hubNodesOf(t *testing.T, result any) []graph.HubNode {
	t.Helper()
	res, ok := result.(*CallToolResult)
	if !ok {
		t.Fatalf("expected *CallToolResult, got %T", result)
	}
	var hubs []graph.HubNode
	if err := json.Unmarshal([]byte(res.Content[0].Text), &hubs); err != nil {
		t.Fatalf("unmarshal hub nodes: %v", err)
	}
	return hubs
}

func TestReadHubNodesHandler(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	readH := readHubNodesHandler(mgr)

	seed := `{
		"topic":"hubs",
		"nodes":[
			{"node_id":"decision","type":"decision","summary":"decision","confidence":0},
			{"node_id":"f1","type":"finding","summary":"finding 1"},
			{"node_id":"f2","type":"finding","summary":"finding 2"},
			{"node_id":"alone","type":"finding","summary":"alone"}
		],
		"edges":[
			{"edge_id":"e1","type":"supports","from_node_id":"decision","to_node_id":"f1"},
			{"edge_id":"e2","type":"supports","from_node_id":"decision","to_node_id":"f2"}
		]
	}`
	if _, rpcErr := writeH(json.RawMessage(seed)); rpcErr != nil {
		t.Fatalf("seed: %v", rpcErr)
	}

	result, rpcErr := readH(json.RawMessage(`{"topic":"hubs"}`))
	if rpcErr != nil {
		t.Fatalf("read hubs: %v", rpcErr)
	}
	hubs := hubNodesOf(t, result)
	if len(hubs) != 3 || hubs[0].NodeID != "decision" || hubs[0].NeighborCount != 2 {
		t.Fatalf("unexpected hubs: %+v", hubs)
	}
	if hubs[0].Confidence == nil || *hubs[0].Confidence != 0 {
		t.Fatalf("explicit zero confidence missing: %+v", hubs[0])
	}

	result, rpcErr = readH(json.RawMessage(`{"topic":"hubs","types":["decision"]}`))
	if rpcErr != nil {
		t.Fatalf("filtered hubs: %v", rpcErr)
	}
	hubs = hubNodesOf(t, result)
	if len(hubs) != 1 || hubs[0].NodeID != "decision" {
		t.Fatalf("unexpected filtered hubs: %+v", hubs)
	}
}

func TestReadHubNodesHandlerValidationAndEmptyJSON(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	h := readHubNodesHandler(mgr)

	for name, params := range map[string]string{
		"missing topic":  `{}`,
		"zero limit":     `{"topic":"t","limit":0}`,
		"large limit":    `{"topic":"t","limit":51}`,
		"empty types":    `{"topic":"t","types":[]}`,
		"mixed wildcard": `{"topic":"t","types":["*","finding"]}`,
		"unknown type":   `{"topic":"t","types":["bogus"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, rpcErr := h(json.RawMessage(params)); rpcErr == nil {
				t.Fatalf("expected validation error for %s", params)
			}
		})
	}

	result, rpcErr := h(json.RawMessage(`{"topic":"empty","types":["finding","finding"]}`))
	if rpcErr != nil {
		t.Fatalf("duplicate named types should be accepted defensively: %v", rpcErr)
	}
	res := result.(*CallToolResult)
	if res.Content[0].Text != "[]" {
		t.Fatalf("empty payload = %q, want []", res.Content[0].Text)
	}
}

func TestReadHubNodesRegistered(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	s := NewServer(mgr)
	for _, tool := range s.toolDefs {
		if tool.Name != "read_hub_nodes" {
			continue
		}
		if !strings.Contains(string(tool.InputSchema), `"maximum": 50`) || !strings.Contains(string(tool.InputSchema), `"minItems": 1`) {
			t.Fatalf("unexpected read_hub_nodes schema: %s", tool.InputSchema)
		}
		return
	}
	t.Fatal("read_hub_nodes not registered")
}

func BenchmarkReadHubNodesHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	g, err := mgr.Topic("bench-hubs")
	if err != nil {
		b.Fatal(err)
	}
	const nodeCount = 1000
	nodes := make([]*graph.Node, nodeCount)
	edges := make([]*graph.Edge, 0, nodeCount-1)
	for i := range nodes {
		nodes[i] = &graph.Node{NodeID: fmt.Sprintf("n-%04d", i), Type: graph.NodeTypeFinding, Summary: "node"}
		if i > 0 {
			edges = append(edges, &graph.Edge{EdgeID: fmt.Sprintf("e-%04d", i), Type: graph.EdgeTypeReferences, FromNodeID: nodes[i-1].NodeID, ToNodeID: nodes[i].NodeID})
		}
	}
	if _, err := g.Write(nodes, edges); err != nil {
		b.Fatal(err)
	}
	h := readHubNodesHandler(mgr)
	params := json.RawMessage(`{"topic":"bench-hubs","limit":10}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, rpcErr := h(params); rpcErr != nil {
			b.Fatal(rpcErr)
		}
	}
}

func TestReadNodesSinceCursorHandler_LimitBounds(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	handler := readNodesSinceCursorHandler(mgr)

	call := func(t *testing.T, params string) (any, *RPCError) {
		t.Helper()
		return handler(json.RawMessage(params))
	}

	t.Run("limit 0 is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "limit": 0}`)
		if rpcErr == nil {
			t.Fatal("expected error for limit=0, got nil")
		}
	})

	t.Run("negative limit is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "limit": -1}`)
		if rpcErr == nil {
			t.Fatal("expected error for limit=-1, got nil")
		}
	})

	t.Run("limit above max is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "limit": 1001}`)
		if rpcErr == nil {
			t.Fatal("expected error for limit=1001, got nil")
		}
	})

	t.Run("limit at max is accepted", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "limit": 1000}`)
		if rpcErr != nil {
			t.Fatalf("expected no error for limit=1000, got %v", rpcErr)
		}
	})

	t.Run("omitted limit defaults to 100", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1"}`)
		if rpcErr != nil {
			t.Fatalf("expected no error for omitted limit, got %v", rpcErr)
		}
	})
}

func TestReadNodesSinceCursorHandler_TypesFilter(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	readHandler := readNodesSinceCursorHandler(mgr)
	writeH := writeHandler(mgr)

	call := func(t *testing.T, params string) (any, *RPCError) {
		t.Helper()
		return readHandler(json.RawMessage(params))
	}

	summariesOf := func(t *testing.T, result any) []graph.NodeSummary {
		t.Helper()
		res, ok := result.(*CallToolResult)
		if !ok {
			t.Fatalf("expected *CallToolResult, got %T", result)
		}
		var summaries []graph.NodeSummary
		if err := json.Unmarshal([]byte(res.Content[0].Text), &summaries); err != nil {
			t.Fatalf("failed to unmarshal summaries: %v", err)
		}
		return summaries
	}

	t.Run("unknown type is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "types": ["bogus"]}`)
		if rpcErr == nil {
			t.Fatal("expected error for unknown type, got nil")
		}
	})

	t.Run("valid single type is accepted", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "types": ["decision"]}`)
		if rpcErr != nil {
			t.Fatalf("expected no error for types=[\"decision\"], got %v", rpcErr)
		}
	})

	writeParams := `{
		"topic": "t2",
		"nodes": [
			{"node_id": "f1", "type": "finding", "summary": "finding one"},
			{"node_id": "f2", "type": "finding", "summary": "finding two"},
			{"node_id": "f3", "type": "finding", "summary": "finding three"},
			{"node_id": "d1", "type": "decision", "summary": "decision one"},
			{"node_id": "d2", "type": "decision", "summary": "decision two"}
		]
	}`
	if _, rpcErr := writeH(json.RawMessage(writeParams)); rpcErr != nil {
		t.Fatalf("failed to seed nodes: %v", rpcErr)
	}

	t.Run("omitted types defaults to finding only", func(t *testing.T) {
		result, rpcErr := call(t, `{"topic": "t2"}`)
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		summaries := summariesOf(t, result)
		if len(summaries) != 3 {
			t.Fatalf("expected 3 finding nodes, got %d", len(summaries))
		}
		for _, s := range summaries {
			if s.Type != graph.NodeTypeFinding {
				t.Fatalf("expected type finding, got %s", s.Type)
			}
		}
	})

	t.Run("types filter with limit counts post-filter matches", func(t *testing.T) {
		result, rpcErr := call(t, `{"topic": "t2", "types": ["decision"], "limit": 1}`)
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		summaries := summariesOf(t, result)
		if len(summaries) != 1 {
			t.Fatalf("expected exactly 1 decision node, got %d", len(summaries))
		}
		if summaries[0].Type != graph.NodeTypeDecision {
			t.Fatalf("expected type decision, got %s", summaries[0].Type)
		}
	})

	t.Run("wildcard returns all types", func(t *testing.T) {
		result, rpcErr := call(t, `{"topic": "t2", "types": ["*"]}`)
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		summaries := summariesOf(t, result)
		if len(summaries) != 5 {
			t.Fatalf("expected all 5 nodes, got %d", len(summaries))
		}
	})
}

func TestReadNodesSinceCursorHandler_NegativeOneSentinel(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	readHandler := readNodesSinceCursorHandler(mgr)
	writeH := writeHandler(mgr)

	call := func(t *testing.T, params string) (any, *RPCError) {
		t.Helper()
		return readHandler(json.RawMessage(params))
	}
	summariesOf := func(t *testing.T, result any) []graph.NodeSummary {
		t.Helper()
		res, ok := result.(*CallToolResult)
		if !ok {
			t.Fatalf("expected *CallToolResult, got %T", result)
		}
		var summaries []graph.NodeSummary
		if err := json.Unmarshal([]byte(res.Content[0].Text), &summaries); err != nil {
			t.Fatalf("failed to unmarshal summaries: %v", err)
		}
		return summaries
	}

	seed := `{"topic":"t1","nodes":[
		{"node_id":"f1","type":"finding","summary":"one"},
		{"node_id":"f2","type":"finding","summary":"two"}
	]}`
	if _, rpcErr := writeH(json.RawMessage(seed)); rpcErr != nil {
		t.Fatalf("failed to seed nodes: %v", rpcErr)
	}

	t.Run("cursor=-1 returns newest nodes first", func(t *testing.T) {
		result, rpcErr := call(t, `{"topic": "t1", "cursor": -1, "limit": 1}`)
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		got := summariesOf(t, result)
		if len(got) != 1 || got[0].NodeID != "f2" {
			t.Fatalf("expected exactly [f2], got %+v", got)
		}
	})

	t.Run("cursor=-1 overrides ascending sort", func(t *testing.T) {
		result, rpcErr := call(t, `{"topic": "t1", "cursor": -1, "sort": "ascending"}`)
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		got := summariesOf(t, result)
		if len(got) != 2 || got[0].NodeID != "f2" || got[1].NodeID != "f1" {
			t.Fatalf("expected [f2 f1], got %+v", got)
		}
	})

	t.Run("explicit cursor at the pre-write head sees only the new node", func(t *testing.T) {
		if _, rpcErr := writeH(json.RawMessage(`{"topic":"t1","nodes":[{"node_id":"f3","type":"finding","summary":"three"}]}`)); rpcErr != nil {
			t.Fatalf("failed to seed third node: %v", rpcErr)
		}
		result, rpcErr := call(t, `{"topic": "t1", "cursor": 2}`)
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		got := summariesOf(t, result)
		if len(got) != 1 || got[0].NodeID != "f3" {
			t.Fatalf("expected exactly [f3], got %+v", got)
		}
	})
}

func TestReadNodesSinceCursorHandler_SortDescending(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	readHandler := readNodesSinceCursorHandler(mgr)
	writeH := writeHandler(mgr)

	seed := `{"topic":"t1","nodes":[
		{"node_id":"f1","type":"finding","summary":"one"},
		{"node_id":"f2","type":"finding","summary":"two"},
		{"node_id":"f3","type":"finding","summary":"three"}
	]}`
	if _, rpcErr := writeH(json.RawMessage(seed)); rpcErr != nil {
		t.Fatalf("failed to seed nodes: %v", rpcErr)
	}

	result, rpcErr := readHandler(json.RawMessage(`{"topic": "t1", "sort": "descending"}`))
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	res, ok := result.(*CallToolResult)
	if !ok {
		t.Fatalf("expected *CallToolResult, got %T", result)
	}
	var summaries []graph.NodeSummary
	if err := json.Unmarshal([]byte(res.Content[0].Text), &summaries); err != nil {
		t.Fatalf("failed to unmarshal summaries: %v", err)
	}
	want := []string{"f3", "f2", "f1"}
	if len(summaries) != len(want) {
		t.Fatalf("expected %d nodes, got %d: %+v", len(want), len(summaries), summaries)
	}
	for i, id := range want {
		if summaries[i].NodeID != id {
			t.Fatalf("expected descending order %v, got %+v", want, summaries)
		}
	}
}

func TestReadNodesSinceCursorHandler_InvalidSort(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	handler := readNodesSinceCursorHandler(mgr)

	_, rpcErr := handler(json.RawMessage(`{"topic": "t1", "sort": "sideways"}`))
	if rpcErr == nil {
		t.Fatal("expected error for sort=\"sideways\", got nil")
	}
}

func TestReadNodesSinceDateHandler_ValidFormats(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	dateHandler := readNodesSinceDateHandler(mgr)
	writeH := writeHandler(mgr)

	if _, rpcErr := writeH(json.RawMessage(`{"topic":"t1","nodes":[{"node_id":"f1","type":"finding","summary":"one"}]}`)); rpcErr != nil {
		t.Fatalf("failed to seed node: %v", rpcErr)
	}

	summariesOf := func(t *testing.T, result any) []graph.NodeSummary {
		t.Helper()
		res, ok := result.(*CallToolResult)
		if !ok {
			t.Fatalf("expected *CallToolResult, got %T", result)
		}
		var summaries []graph.NodeSummary
		if err := json.Unmarshal([]byte(res.Content[0].Text), &summaries); err != nil {
			t.Fatalf("failed to unmarshal summaries: %v", err)
		}
		return summaries
	}

	t.Run("RFC3339 date far in the past includes everything", func(t *testing.T) {
		result, rpcErr := dateHandler(json.RawMessage(`{"topic": "t1", "date": "2000-01-01T00:00:00Z"}`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if got := summariesOf(t, result); len(got) != 1 {
			t.Fatalf("expected 1 node, got %d: %+v", len(got), got)
		}
	})

	t.Run("plain YYYY-MM-DD date far in the past includes everything", func(t *testing.T) {
		result, rpcErr := dateHandler(json.RawMessage(`{"topic": "t1", "date": "2000-01-01"}`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if got := summariesOf(t, result); len(got) != 1 {
			t.Fatalf("expected 1 node, got %d: %+v", len(got), got)
		}
	})

	t.Run("date far in the future returns nothing", func(t *testing.T) {
		result, rpcErr := dateHandler(json.RawMessage(`{"topic": "t1", "date": "2099-01-01"}`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if got := summariesOf(t, result); len(got) != 0 {
			t.Fatalf("expected 0 nodes, got %d: %+v", len(got), got)
		}
	})
}

func TestReadNodesSinceDateHandler_InvalidDate(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	handler := readNodesSinceDateHandler(mgr)

	for _, tc := range []struct{ name, params string }{
		{"missing date", `{"topic": "t1"}`},
		{"garbage date", `{"topic": "t1", "date": "not-a-date"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rpcErr := handler(json.RawMessage(tc.params))
			if rpcErr == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestReadNodesSinceDateHandler_TypesAndSort(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	dateHandler := readNodesSinceDateHandler(mgr)
	writeH := writeHandler(mgr)

	seed := `{"topic":"t1","nodes":[
		{"node_id":"f1","type":"finding","summary":"one"},
		{"node_id":"d1","type":"decision","summary":"two"},
		{"node_id":"f2","type":"finding","summary":"three"}
	]}`
	if _, rpcErr := writeH(json.RawMessage(seed)); rpcErr != nil {
		t.Fatalf("failed to seed nodes: %v", rpcErr)
	}

	result, rpcErr := dateHandler(json.RawMessage(`{"topic": "t1", "date": "2000-01-01", "types": ["finding"], "sort": "descending"}`))
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	res, ok := result.(*CallToolResult)
	if !ok {
		t.Fatalf("expected *CallToolResult, got %T", result)
	}
	var summaries []graph.NodeSummary
	if err := json.Unmarshal([]byte(res.Content[0].Text), &summaries); err != nil {
		t.Fatalf("failed to unmarshal summaries: %v", err)
	}
	want := []string{"f2", "f1"}
	if len(summaries) != len(want) {
		t.Fatalf("expected %d finding nodes, got %d: %+v", len(want), len(summaries), summaries)
	}
	for i, id := range want {
		if summaries[i].NodeID != id {
			t.Fatalf("expected descending finding-only order %v, got %+v", want, summaries)
		}
	}
}

func TestNeighborsHandler_Validation(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	handler := neighborsHandler(mgr)

	call := func(t *testing.T, params string) (any, *RPCError) {
		t.Helper()
		return handler(json.RawMessage(params))
	}

	seed := `{
		"topic": "t1",
		"nodes": [
			{"node_id": "a", "type": "finding", "summary": "a"},
			{"node_id": "b", "type": "finding", "summary": "b"}
		],
		"edges": [
			{"edge_id": "e1", "type": "references", "from_node_id": "a", "to_node_id": "b"}
		]
	}`
	if _, rpcErr := writeH(json.RawMessage(seed)); rpcErr != nil {
		t.Fatalf("failed to seed graph: %v", rpcErr)
	}

	t.Run("missing topic is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"node_id": "a"}`)
		if rpcErr == nil {
			t.Fatal("expected error for missing topic, got nil")
		}
	})

	t.Run("missing node_id is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1"}`)
		if rpcErr == nil {
			t.Fatal("expected error for missing node_id, got nil")
		}
	})

	t.Run("depth 0 is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "depth": 0}`)
		if rpcErr == nil {
			t.Fatal("expected error for depth=0, got nil")
		}
	})

	t.Run("depth above max is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "depth": 4}`)
		if rpcErr == nil {
			t.Fatal("expected error for depth=4, got nil")
		}
	})

	t.Run("invalid direction is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "direction": "sideways"}`)
		if rpcErr == nil {
			t.Fatal("expected error for invalid direction, got nil")
		}
	})

	t.Run("invalid edge type is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "edge_types": ["bogus"]}`)
		if rpcErr == nil {
			t.Fatal("expected error for invalid edge type, got nil")
		}
	})

	t.Run("limit 0 is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "limit": 0}`)
		if rpcErr == nil {
			t.Fatal("expected error for limit=0, got nil")
		}
	})

	t.Run("limit above max is rejected", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "limit": 201}`)
		if rpcErr == nil {
			t.Fatal("expected error for limit=201, got nil")
		}
	})

	t.Run("limit at max is accepted", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "a", "limit": 200}`)
		if rpcErr != nil {
			t.Fatalf("expected no error for limit=200, got %v", rpcErr)
		}
	})

	t.Run("all params omitted uses defaults", func(t *testing.T) {
		result, rpcErr := call(t, `{"topic": "t1", "node_id": "a"}`)
		if rpcErr != nil {
			t.Fatalf("expected no error with defaults, got %v", rpcErr)
		}
		res, ok := result.(*CallToolResult)
		if !ok {
			t.Fatalf("expected *CallToolResult, got %T", result)
		}
		var nr graph.NeighborsResult
		if err := json.Unmarshal([]byte(res.Content[0].Text), &nr); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		if len(nr.Neighbors) != 1 || nr.Neighbors[0].NodeID != "b" {
			t.Fatalf("expected neighbor b at default depth 1, got %+v", nr.Neighbors)
		}
	})

	t.Run("unknown node_id returns not found error", func(t *testing.T) {
		_, rpcErr := call(t, `{"topic": "t1", "node_id": "missing"}`)
		if rpcErr == nil {
			t.Fatal("expected error for unknown node_id, got nil")
		}
	})
}

// BenchmarkWriteHandler measures the full path a real MCP client pays for:
// JSON-unmarshaling the tool arguments, running the write, and
// JSON-marshaling the result — not just the underlying graph.Write cost.
func BenchmarkWriteHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	handler := writeHandler(mgr)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		params := json.RawMessage(fmt.Sprintf(
			`{"topic":"bench","nodes":[{"node_id":"n%d","type":"finding","summary":"x"}]}`, i))
		if _, rpcErr := handler(params); rpcErr != nil {
			b.Fatalf("write: %v", rpcErr)
		}
	}
}

// BenchmarkReadNodesSinceCursorHandler measures the full JSON-RPC path for
// the default (types=["finding"]) read_nodes_since_cursor call against a
// modest, pre-seeded topic.
func BenchmarkReadNodesSinceCursorHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	readH := readNodesSinceCursorHandler(mgr)

	const n = 1000
	nodes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"node_id":"n%d","type":"finding","summary":"x"}`, i))
	}
	seed := json.RawMessage(fmt.Sprintf(`{"topic":"bench","nodes":[%s]}`, strings.Join(nodes, ",")))
	if _, rpcErr := writeH(seed); rpcErr != nil {
		b.Fatalf("seed write: %v", rpcErr)
	}

	params := json.RawMessage(`{"topic":"bench","limit":100}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, rpcErr := readH(params); rpcErr != nil {
			b.Fatalf("read: %v", rpcErr)
		}
	}
}

// BenchmarkReadNodesSinceDateHandler measures the full JSON-RPC path for
// read_nodes_since_date against the same modest, pre-seeded topic as
// BenchmarkReadNodesSinceCursorHandler, for a direct comparison.
func BenchmarkReadNodesSinceDateHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	dateH := readNodesSinceDateHandler(mgr)

	const n = 1000
	nodes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"node_id":"n%d","type":"finding","summary":"x"}`, i))
	}
	seed := json.RawMessage(fmt.Sprintf(`{"topic":"bench","nodes":[%s]}`, strings.Join(nodes, ",")))
	if _, rpcErr := writeH(seed); rpcErr != nil {
		b.Fatalf("seed write: %v", rpcErr)
	}

	params := json.RawMessage(`{"topic":"bench","date":"2000-01-01","limit":100}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, rpcErr := dateH(params); rpcErr != nil {
			b.Fatalf("read: %v", rpcErr)
		}
	}
}

// BenchmarkNeighborsHandler measures the full JSON-RPC path for a neighbors
// call against a chain-shaped topic, mirroring BenchmarkGetNode's fixture.
func BenchmarkNeighborsHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	neighborsH := neighborsHandler(mgr)

	const n = 1000
	nodes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"node_id":"n%d","type":"finding","summary":"x"}`, i))
	}
	edges := make([]string, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, fmt.Sprintf(`{"edge_id":"e%d","type":"references","from_node_id":"n%d","to_node_id":"n%d"}`, i, i, i+1))
	}
	seed := json.RawMessage(fmt.Sprintf(`{"topic":"bench","nodes":[%s],"edges":[%s]}`, strings.Join(nodes, ","), strings.Join(edges, ",")))
	if _, rpcErr := writeH(seed); rpcErr != nil {
		b.Fatalf("seed write: %v", rpcErr)
	}

	params := json.RawMessage(`{"topic":"bench","node_id":"n500","depth":2,"limit":50}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, rpcErr := neighborsH(params); rpcErr != nil {
			b.Fatalf("neighbors: %v", rpcErr)
		}
	}
}
