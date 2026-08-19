package graph

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func float64Ptr(v float64) *float64 { return &v }

func seedHubGraph(t *testing.T, nodes []*Node, edges []*Edge) *TopicGraph {
	t.Helper()
	g, _ := newTestGraph(t)
	if _, err := g.Write(nodes, edges); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return g
}

func TestHubNodesDistinctUndirectedDegreeAndOrder(t *testing.T) {
	g := seedHubGraph(t,
		[]*Node{
			{NodeID: "center", Type: NodeTypeDecision, Summary: "center"},
			{NodeID: "a", Type: NodeTypeFinding, Summary: "a"},
			{NodeID: "b", Type: NodeTypeFinding, Summary: "b"},
			{NodeID: "c", Type: NodeTypeFinding, Summary: "c"},
			{NodeID: "isolated", Type: NodeTypeDecision, Summary: "isolated"},
		},
		[]*Edge{
			{EdgeID: "e1", Type: EdgeTypeSupports, FromNodeID: "center", ToNodeID: "a"},
			{EdgeID: "e2", Type: EdgeTypeReferences, FromNodeID: "a", ToNodeID: "center"},
			{EdgeID: "e3", Type: EdgeTypeSupports, FromNodeID: "center", ToNodeID: "b"},
			{EdgeID: "e4", Type: EdgeTypeSupports, FromNodeID: "center", ToNodeID: "c"},
			{EdgeID: "loop", Type: EdgeTypeReferences, FromNodeID: "center", ToNodeID: "center"},
		})

	got := g.HubNodes(10, nil)
	if len(got) != 4 {
		t.Fatalf("expected four connected nodes, got %+v", got)
	}
	if got[0].NodeID != "center" || got[0].NeighborCount != 3 {
		t.Fatalf("expected center degree 3 first, got %+v", got[0])
	}
	// Leaves tie on degree, so newest seq wins.
	wantLeaves := []string{"c", "b", "a"}
	for i, want := range wantLeaves {
		if got[i+1].NodeID != want || got[i+1].NeighborCount != 1 {
			t.Fatalf("result %d = %+v, want leaf %s degree 1", i+1, got[i+1], want)
		}
	}
}

func TestHubNodesTypeFilterUsesAllNeighbors(t *testing.T) {
	g := seedHubGraph(t,
		[]*Node{
			{NodeID: "decision", Type: NodeTypeDecision, Summary: "decision"},
			{NodeID: "finding-1", Type: NodeTypeFinding, Summary: "finding 1"},
			{NodeID: "finding-2", Type: NodeTypeFinding, Summary: "finding 2"},
		},
		[]*Edge{
			{EdgeID: "e1", Type: EdgeTypeSupports, FromNodeID: "decision", ToNodeID: "finding-1"},
			{EdgeID: "e2", Type: EdgeTypeSupports, FromNodeID: "decision", ToNodeID: "finding-2"},
		})

	got := g.HubNodes(10, []NodeType{NodeTypeDecision})
	if len(got) != 1 || got[0].NodeID != "decision" || got[0].NeighborCount != 2 {
		t.Fatalf("candidate-only type filter returned %+v", got)
	}
}

func TestHubNodesEmptyIsNonNil(t *testing.T) {
	g := seedHubGraph(t, []*Node{{NodeID: "alone", Type: NodeTypeFinding, Summary: "alone"}}, nil)
	got := g.HubNodes(10, nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("expected non-nil empty result, got %#v", got)
	}
	data, err := json.Marshal(got)
	if err != nil || string(data) != "[]" {
		t.Fatalf("marshal = %q, %v; want []", data, err)
	}
}

func TestHubNodesConfidenceOutputAndRankingIndependence(t *testing.T) {
	zero := 0.0
	one := 1.0
	g := seedHubGraph(t,
		[]*Node{
			{NodeID: "absent", Type: NodeTypeFinding, Summary: "absent"},
			{NodeID: "zero", Type: NodeTypeFinding, Summary: "zero", Confidence: &zero},
			{NodeID: "one", Type: NodeTypeFinding, Summary: "one", Confidence: &one},
		},
		[]*Edge{
			{EdgeID: "e1", Type: EdgeTypeReferences, FromNodeID: "absent", ToNodeID: "zero"},
			{EdgeID: "e2", Type: EdgeTypeReferences, FromNodeID: "zero", ToNodeID: "one"},
			{EdgeID: "e3", Type: EdgeTypeReferences, FromNodeID: "one", ToNodeID: "absent"},
		})

	got := g.HubNodes(3, nil)
	// All degrees tie; seq alone determines order despite confidence values.
	if ids := []string{got[0].NodeID, got[1].NodeID, got[2].NodeID}; !reflect.DeepEqual(ids, []string{"one", "zero", "absent"}) {
		t.Fatalf("unexpected confidence-influenced order: %v", ids)
	}
	if got[0].Confidence == nil || *got[0].Confidence != 1 || got[1].Confidence == nil || *got[1].Confidence != 0 || got[2].Confidence != nil {
		t.Fatalf("confidence metadata not preserved: %+v", got)
	}
	one = 0.25
	if *got[0].Confidence != 1 {
		t.Fatal("returned confidence aliases source value")
	}
}

func TestRankHubNodesIgnoresDanglingEndpoints(t *testing.T) {
	snapshot := hubSnapshot{
		nodes: []hubSnapshotNode{{nodeID: "a", typeName: NodeTypeFinding, seq: 1}},
		edges: []nodePair{{a: "a", b: "missing"}},
	}
	got := rankHubNodes(snapshot, 10, nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("expected dangling edge to be ignored, got %+v", got)
	}
}

func referenceHubNodes(snapshot hubSnapshot, limit int) []HubNode {
	all := rankHubNodes(snapshot, len(snapshot.nodes), nil)
	sort.Slice(all, func(i, j int) bool { return betterHub(all[i], all[j]) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func TestHubHeapMatchesFullSelection(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 100; iteration++ {
		n := 5 + rng.Intn(100)
		snapshot := hubSnapshot{nodes: make([]hubSnapshotNode, n)}
		for i := range snapshot.nodes {
			snapshot.nodes[i] = hubSnapshotNode{nodeID: fmt.Sprintf("n-%03d", i), typeName: NodeTypeFinding, seq: int64(rng.Intn(20))}
		}
		for i := 0; i < n*3; i++ {
			snapshot.edges = append(snapshot.edges, nodePair{a: snapshot.nodes[rng.Intn(n)].nodeID, b: snapshot.nodes[rng.Intn(n)].nodeID})
		}
		for _, limit := range []int{1, 3, 10, 50} {
			got := rankHubNodes(snapshot, limit, nil)
			want := referenceHubNodes(snapshot, limit)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("iteration %d limit %d:\ngot  %+v\nwant %+v", iteration, limit, got, want)
			}
		}
	}
}

func TestOptionalConfidenceWALCompatibility(t *testing.T) {
	var missing Node
	if err := json.Unmarshal([]byte(`{"node_id":"missing","type":"finding","summary":"missing"}`), &missing); err != nil {
		t.Fatal(err)
	}
	var zero Node
	if err := json.Unmarshal([]byte(`{"node_id":"zero","type":"finding","summary":"zero","confidence":0}`), &zero); err != nil {
		t.Fatal(err)
	}
	if missing.Confidence != nil || zero.Confidence == nil || *zero.Confidence != 0 {
		t.Fatalf("missing=%v zero=%v", missing.Confidence, zero.Confidence)
	}
}

func TestOptionalConfidenceValidation(t *testing.T) {
	g, _ := newTestGraph(t)
	for _, confidence := range []float64{-0.1, 1.1} {
		_, err := g.Write([]*Node{{NodeID: fmt.Sprintf("n-%g", confidence), Type: NodeTypeFinding, Summary: "invalid", Confidence: float64Ptr(confidence)}}, nil)
		if err == nil {
			t.Fatalf("expected confidence %g to be rejected", confidence)
		}
	}
	if _, err := g.Write([]*Node{{NodeID: "unknown", Type: NodeTypeFinding, Summary: "unknown"}}, nil); err != nil {
		t.Fatalf("absent confidence rejected: %v", err)
	}
}

func TestHubNodesConcurrentWrite(t *testing.T) {
	g := seedHubGraph(t,
		[]*Node{{NodeID: "root", Type: NodeTypeConcept, Summary: "root"}},
		nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			id := fmt.Sprintf("n-%02d", i)
			if _, err := g.Write(
				[]*Node{{NodeID: id, Type: NodeTypeFinding, Summary: id}},
				[]*Edge{{EdgeID: "e-" + id, Type: EdgeTypeReferences, FromNodeID: "root", ToNodeID: id}},
			); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = g.HubNodes(10, nil)
		}
	}()
	wg.Wait()
	got := g.HubNodes(1, nil)
	if len(got) != 1 || got[0].NodeID != "root" || got[0].NeighborCount != 50 {
		t.Fatalf("unexpected final hub: %+v", got)
	}
}

func BenchmarkHubNodesSparse(b *testing.B) {
	g, err := newTopicGraph(b.TempDir() + "/graph.jsonl")
	if err != nil {
		b.Fatal(err)
	}
	defer g.Close()
	const nodeCount = 10000
	nodes := make([]*Node, nodeCount)
	edges := make([]*Edge, 0, nodeCount-1)
	for i := range nodes {
		nodes[i] = &Node{NodeID: fmt.Sprintf("n-%05d", i), Type: NodeTypeFinding, Summary: "node"}
		if i > 0 {
			edges = append(edges, &Edge{EdgeID: fmt.Sprintf("e-%05d", i), Type: EdgeTypeReferences, FromNodeID: nodes[i-1].NodeID, ToNodeID: nodes[i].NodeID})
		}
	}
	if _, err := g.Write(nodes, edges); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.HubNodes(10, nil)
	}
}
