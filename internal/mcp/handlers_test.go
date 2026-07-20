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
