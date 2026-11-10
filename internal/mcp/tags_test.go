package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

func toolText(t testing.TB, result any) string {
	t.Helper()
	res, ok := result.(*CallToolResult)
	if !ok || len(res.Content) != 1 {
		t.Fatalf("unexpected tool result: %#v", result)
	}
	return res.Content[0].Text
}

func TestTagHandlers(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	writeH := writeHandler(mgr)
	if _, rpcErr := writeH(json.RawMessage(`{"topic":"tags","nodes":[
		{"node_id":"n1","type":"finding","summary":"one","tags":["graph","search"]},
		{"node_id":"n2","type":"decision","summary":"two","tags":["graph","design"]}
	]}`)); rpcErr != nil {
		t.Fatal(rpcErr)
	}

	result, rpcErr := searchByTagsHandler(mgr)(json.RawMessage(`{"topic":"tags","tags":["graph"],"types":["decision"]}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var matches []graph.TaggedNodeSummary
	if err := json.Unmarshal([]byte(toolText(t, result)), &matches); err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].NodeID != "n2" || len(matches[0].Tags) != 2 {
		t.Fatalf("matches = %+v", matches)
	}

	result, rpcErr = listTagsHandler(mgr)(json.RawMessage(`{"topic":"tags","limit":2}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var counts []graph.TagCount
	if err := json.Unmarshal([]byte(toolText(t, result)), &counts); err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 || counts[0].Tag != "graph" || counts[0].NodeCount != 2 || counts[1].Tag != "design" {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestTagHandlerValidationAndEmptyJSON(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	search := searchByTagsHandler(mgr)
	for _, params := range []string{
		`{}`, `{"topic":"t","tags":[]}`, `{"topic":"t","tags":["Upper"]}`,
		`{"topic":"t","tags":["same","same"]}`, `{"topic":"t","tags":["ok"],"match":"some"}`,
		`{"topic":"t","tags":["ok"],"limit":0}`, `{"topic":"t","tags":["ok"],"types":[]}`,
	} {
		if _, rpcErr := search(json.RawMessage(params)); rpcErr == nil {
			t.Fatalf("expected validation error for %s", params)
		}
	}
	if _, rpcErr := writeHandler(mgr)(json.RawMessage(`{"topic":"empty"}`)); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	result, rpcErr := search(json.RawMessage(`{"topic":"empty","tags":["missing"]}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := toolText(t, result); got != "[]" {
		t.Fatalf("empty payload = %q", got)
	}

	list := listTagsHandler(mgr)
	for _, params := range []string{`{}`, `{"topic":"t","limit":0}`, `{"topic":"t","limit":1001}`} {
		if _, rpcErr := list(json.RawMessage(params)); rpcErr == nil {
			t.Fatalf("expected validation error for %s", params)
		}
	}
}

func TestTagToolsRegistered(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	s := NewServer(mgr)
	found := map[string]bool{}
	for _, tool := range s.toolDefs {
		if tool.Name == "search_by_tags" || tool.Name == "list_tags" {
			found[tool.Name] = true
			if !strings.Contains(string(tool.InputSchema), `"required"`) {
				t.Fatalf("schema missing required: %s", tool.InputSchema)
			}
		}
	}
	if !found["search_by_tags"] || !found["list_tags"] {
		t.Fatalf("registered = %v", found)
	}
}

func BenchmarkSearchByTagsHandler(b *testing.B) {
	mgr := graph.NewManager(b.TempDir())
	defer mgr.Close()
	g, release, err := mgr.OpenTopic("bench-tags", true)
	if err != nil {
		b.Fatal(err)
	}
	nodes := make([]*graph.Node, 1000)
	for i := range nodes {
		nodes[i] = &graph.Node{NodeID: string(rune(i + 1)), Type: graph.NodeTypeFinding, Summary: "node", Tags: []string{"common"}}
	}
	_, err = g.Write(nodes, nil)
	release()
	if err != nil {
		b.Fatal(err)
	}
	h := searchByTagsHandler(mgr)
	params := json.RawMessage(`{"topic":"bench-tags","tags":["common"]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, rpcErr := h(params); rpcErr != nil {
			b.Fatal(rpcErr)
		}
	}
}
