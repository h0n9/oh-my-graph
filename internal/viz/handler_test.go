package viz

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

func TestGraphDataIncludesNodeTagsOnly(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	g, err := mgr.Topic("tags")
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Write(
		[]*graph.Node{
			{NodeID: "a", Type: graph.NodeTypeFinding, Summary: "a", Tags: []string{"graph", "search"}},
			{NodeID: "b", Type: graph.NodeTypeFinding, Summary: "b"},
		},
		[]*graph.Edge{{EdgeID: "e", Type: graph.EdgeTypeReferences, FromNodeID: "a", ToNodeID: "b"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/graph?topic=tags", nil)
	rec := httptest.NewRecorder()
	NewHandler(mgr).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var data struct {
		Nodes []nodeView        `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Nodes) != 2 {
		t.Fatalf("nodes = %+v", data.Nodes)
	}
	found := false
	for _, n := range data.Nodes {
		if n.ID == "a" && len(n.Tags) == 2 && n.Tags[0] == "graph" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tagged node missing: %+v", data.Nodes)
	}
	if strings.Contains(string(data.Edges[0]), "tags") {
		t.Fatalf("edge unexpectedly has tags: %s", data.Edges[0])
	}
}

func TestGraphPageRendersTagLabels(t *testing.T) {
	req := httptest.NewRequest("GET", "/graph", nil)
	rec := httptest.NewRecorder()
	NewHandler(graph.NewManager(t.TempDir())).ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `class="d-tags"`) || !strings.Contains(body, "n.tags?.length") {
		t.Fatal("graph page does not render tag labels")
	}
}
