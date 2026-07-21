package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

func TestReadNodesSinceHandler_LimitBounds(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	handler := readNodesSinceHandler(mgr)

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

func TestReadNodesSinceHandler_TypesFilter(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	readHandler := readNodesSinceHandler(mgr)
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

// BenchmarkReadNodesSinceHandler measures the full JSON-RPC path for the
// default (types=["finding"]) read_nodes_since call against a modest,
// pre-seeded topic.
func BenchmarkReadNodesSinceHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	readH := readNodesSinceHandler(mgr)

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
