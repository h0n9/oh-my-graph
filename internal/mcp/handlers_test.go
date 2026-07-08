package mcp

import (
	"encoding/json"
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
