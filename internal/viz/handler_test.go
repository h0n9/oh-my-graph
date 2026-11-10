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
	g, release, err := mgr.OpenTopic("tags", true)
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
	release()
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

func TestTopicManagementRoutes(t *testing.T) {
	mgr := graph.NewManager(t.TempDir())
	defer mgr.Close()
	h := NewHandler(mgr)
	seed := func() {
		_, release, err := mgr.OpenTopic("topic", true)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	seed()

	request := func(method, path, body, fetchSite string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
		}
		return rec
	}
	graphInstance := func(name string) string {
		t.Helper()
		rec := request("GET", "/api/graph?topic="+name, "", "", 200)
		var data graphData
		if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil || data.Instance == "" {
			t.Fatalf("graph instance: %q, %v", data.Instance, err)
		}
		return data.Instance
	}

	initialInstance := graphInstance("topic")
	request("GET", "/topics/archive", "", "", 405)
	request("POST", "/topics/archive", "topic=topic", "cross-site", 403)
	if graphInstance("topic") != initialInstance {
		t.Fatal("blocked requests changed graph instance")
	}
	request("POST", "/topics/rename", "topic=topic&new_name=invalid+name", "same-origin", 400)
	request("POST", "/topics/rename", "topic=missing&new_name=missing", "same-origin", 404)
	if got := request("POST", "/topics/rename", "topic=topic&new_name=renamed", "same-origin", 303).Header().Get("Location"); got != "/" {
		t.Fatalf("rename redirect: %s", got)
	}
	request("GET", "/api/graph?topic=topic", "", "", 404)
	seed()
	if graphInstance("topic") == initialInstance {
		t.Fatal("recreated name reused the old graph instance")
	}

	request("POST", "/topics/archive", "topic=renamed", "same-origin", 303)
	request("POST", "/topics/rename", "topic=renamed&new_name=renamed", "same-origin", 409)
	request("GET", "/api/graph?topic=renamed", "", "", 409)
	active := request("GET", "/", "", "", 200).Body.String()
	archive := request("GET", "/?tab=archived", "", "", 200).Body.String()
	if strings.Contains(active, "topic=renamed") || !strings.Contains(archive, `aria-label="Restore renamed"`) ||
		strings.Contains(archive, "topic=renamed") {
		t.Fatal("archived topic must appear only as a restore action")
	}
	if got := request("POST", "/topics/restore", "topic=renamed", "same-origin", 303).Header().Get("Location"); got != "/?tab=archived" {
		t.Fatalf("restore redirect: %s", got)
	}
	graphInstance("renamed")
}
