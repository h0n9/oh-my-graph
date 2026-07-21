package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestGraph(t *testing.T) (*TopicGraph, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		t.Fatalf("newTopicGraph: %v", err)
	}
	t.Cleanup(g.Close)
	return g, path
}

func TestWriteAndReadRoundTrip(t *testing.T) {
	g, _ := newTestGraph(t)

	cursor, err := g.Write([]*Node{
		{NodeID: "n1", Type: NodeTypeFinding, Summary: "s1"},
		{NodeID: "n2", Type: NodeTypeDecision, Summary: "s2"},
	}, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if cursor != 2 {
		t.Fatalf("expected cursor 2, got %d", cursor)
	}

	nwe, ok := g.GetNode("n1")
	if !ok || nwe.Node.Summary != "s1" {
		t.Fatalf("GetNode n1 = %+v, %v", nwe, ok)
	}

	_, nodeCount, _ := g.Stats()
	if nodeCount != 2 {
		t.Fatalf("expected 2 nodes, got %d", nodeCount)
	}
}

func TestNodesSinceSingleTypeMatchesWildcardFilter(t *testing.T) {
	g, _ := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "f1", Type: NodeTypeFinding, Summary: "f1"},
		{NodeID: "d1", Type: NodeTypeDecision, Summary: "d1"},
		{NodeID: "f2", Type: NodeTypeFinding, Summary: "f2"},
	}, nil)

	got := g.NodesSince(0, 100, []NodeType{NodeTypeFinding})
	if len(got) != 2 {
		t.Fatalf("expected 2 finding nodes, got %d: %+v", len(got), got)
	}
	for _, s := range got {
		if s.Type != NodeTypeFinding {
			t.Fatalf("expected only finding nodes, got %s", s.Type)
		}
	}

	// cursor should skip already-seen entries
	got2 := g.NodesSince(got[0].Seq, 100, []NodeType{NodeTypeFinding})
	if len(got2) != 1 || got2[0].NodeID != "f2" {
		t.Fatalf("expected only f2 after cursor, got %+v", got2)
	}
}

func TestNodesSinceMultiTypeMergePreservesSeqOrder(t *testing.T) {
	g, _ := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "d1", Type: NodeTypeDecision, Summary: "d1"},
		{NodeID: "f1", Type: NodeTypeFinding, Summary: "f1"},
		{NodeID: "b1", Type: NodeTypeBlocker, Summary: "b1"},
		{NodeID: "f2", Type: NodeTypeFinding, Summary: "f2"},
	}, nil)

	got := g.NodesSince(0, 100, []NodeType{NodeTypeFinding, NodeTypeDecision})
	if len(got) != 3 {
		t.Fatalf("expected 3 nodes (finding+decision), got %d: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("merged results not in ascending seq order: %+v", got)
		}
	}
	ids := []string{got[0].NodeID, got[1].NodeID, got[2].NodeID}
	want := []string{"d1", "f1", "f2"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("expected order %v, got %v", want, ids)
		}
	}
}

func TestNodesSinceMultiTypeRespectsLimit(t *testing.T) {
	g, _ := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "d1", Type: NodeTypeDecision, Summary: "d1"},
		{NodeID: "f1", Type: NodeTypeFinding, Summary: "f1"},
		{NodeID: "f2", Type: NodeTypeFinding, Summary: "f2"},
	}, nil)

	got := g.NodesSince(0, 2, []NodeType{NodeTypeFinding, NodeTypeDecision})
	if len(got) != 2 {
		t.Fatalf("expected limit=2 to cap results, got %d: %+v", len(got), got)
	}
}

func TestReloadPicksUpExternalAppendIncrementally(t *testing.T) {
	g, path := newTestGraph(t)

	g.Write([]*Node{{NodeID: "n1", Type: NodeTypeFinding, Summary: "s1"}}, nil)
	// wait for the async WAL append + readOffset advance to land
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.RLock()
		off := g.readOffset
		g.mu.RUnlock()
		if off > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	g.mu.RLock()
	offsetAfterOwnWrite := g.readOffset
	g.mu.RUnlock()
	if offsetAfterOwnWrite == 0 {
		t.Fatal("expected readOffset to advance after own write")
	}

	// simulate an external process appending a second record directly to the file
	rec := `{"seq":2,"type":"node","ts":"2024-01-01T00:00:00Z","data":{"node_id":"n2","type":"finding","summary":"s2"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open for external append: %v", err)
	}
	if _, err := f.WriteString(rec); err != nil {
		t.Fatalf("external append: %v", err)
	}
	f.Close()

	g.fileMu.Lock()
	g.mu.Lock()
	g.reloadNewLines()
	g.mu.Unlock()
	g.fileMu.Unlock()

	if _, ok := g.GetNode("n2"); !ok {
		t.Fatal("expected externally appended node n2 to be picked up by reload")
	}
	_, nodeCount, _ := g.Stats()
	if nodeCount != 2 {
		t.Fatalf("expected 2 nodes after reload, got %d", nodeCount)
	}
}

func TestReloadFromScratchOnTruncation(t *testing.T) {
	g, path := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "n1", Type: NodeTypeFinding, Summary: "s1"},
		{NodeID: "n2", Type: NodeTypeFinding, Summary: "s2"},
	}, nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.RLock()
		off := g.readOffset
		g.mu.RUnlock()
		if off > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// simulate truncation/rotation: replace the file with a shorter one
	rec := `{"seq":1,"type":"node","ts":"2024-01-01T00:00:00Z","data":{"node_id":"n3","type":"finding","summary":"s3"}}` + "\n"
	if err := os.WriteFile(path, []byte(rec), 0644); err != nil {
		t.Fatalf("truncate/replace file: %v", err)
	}

	g.fileMu.Lock()
	g.mu.Lock()
	g.reloadNewLines()
	g.mu.Unlock()
	g.fileMu.Unlock()

	_, nodeCount, _ := g.Stats()
	if nodeCount != 1 {
		t.Fatalf("expected rebuild to leave exactly 1 node, got %d", nodeCount)
	}
	if _, ok := g.GetNode("n3"); !ok {
		t.Fatal("expected n3 to be present after rebuild")
	}
	if _, ok := g.GetNode("n1"); ok {
		t.Fatal("expected stale n1 to be gone after rebuild")
	}
}

func TestReopenAfterRestartLoadsPersistedData(t *testing.T) {
	g, path := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "n1", Type: NodeTypeFinding, Summary: "s1"},
	}, nil)
	g.Close()

	// wait for the flush before reopening
	time.Sleep(50 * time.Millisecond)

	g2, err := newTopicGraph(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer g2.Close()

	if _, ok := g2.GetNode("n1"); !ok {
		t.Fatal("expected n1 to survive restart")
	}
}

// TestConcurrentWritesRemainConsistent stresses many overlapping Write calls,
// each of which self-triggers the fsnotify watcher's reload. It guards
// against readOffset desyncing from the actual file layout when a write's
// own bookkeeping races a concurrent reload — a bug that surfaced only under
// concurrent load, not in single-writer tests.
func TestConcurrentWritesRemainConsistent(t *testing.T) {
	g, path := newTestGraph(t)

	const n = 300
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := g.Write([]*Node{{NodeID: fmt.Sprintf("n%d", i), Type: NodeTypeFinding, Summary: "x"}}, nil); err != nil {
				t.Errorf("write %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	g.wg.Wait() // wait for the async WAL append goroutines to finish flushing

	// Give the fsnotify watcher a moment to settle on any trailing reload.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, nodeCount, _ := g.Stats()
		if nodeCount == n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, nodeCount, _ := g.Stats()
	if nodeCount != n {
		t.Fatalf("expected %d nodes after concurrent writes, got %d", n, nodeCount)
	}

	// Reopening from disk must reproduce exactly the same state. If
	// readOffset had desynced from the file layout, the WAL would contain a
	// misaligned/corrupt record and this reload would fail.
	g.Close()
	g2, err := newTopicGraph(path)
	if err != nil {
		t.Fatalf("reopen after concurrent writes: %v", err)
	}
	defer g2.Close()
	_, reopenCount, _ := g2.Stats()
	if reopenCount != n {
		t.Fatalf("expected %d nodes after reopen, got %d", n, reopenCount)
	}
}

func TestNeighborsOneHopBothDirections(t *testing.T) {
	g, _ := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "anchor", Type: NodeTypeConcept, Summary: "anchor"},
		{NodeID: "out1", Type: NodeTypeFinding, Summary: "out1"},
		{NodeID: "in1", Type: NodeTypeFinding, Summary: "in1"},
	}, nil)
	g.Write(nil, []*Edge{
		{EdgeID: "e-out", Type: EdgeTypeReferences, FromNodeID: "anchor", ToNodeID: "out1"},
		{EdgeID: "e-in", Type: EdgeTypeReferences, FromNodeID: "in1", ToNodeID: "anchor"},
	})

	res, ok := g.Neighbors("anchor", 1, "both", nil, 50)
	if !ok {
		t.Fatal("expected anchor to be found")
	}
	if res.Anchor.NodeID != "anchor" || res.Anchor.Type != NodeTypeConcept || res.Anchor.Summary != "anchor" {
		t.Fatalf("unexpected anchor ref: %+v", res.Anchor)
	}
	if len(res.Neighbors) != 2 {
		t.Fatalf("expected 2 neighbors, got %d: %+v", len(res.Neighbors), res.Neighbors)
	}
	byID := map[string]Neighbor{}
	for _, n := range res.Neighbors {
		byID[n.NodeID] = n
		if n.Hop != 1 {
			t.Fatalf("expected hop 1, got %d for %s", n.Hop, n.NodeID)
		}
	}
	if byID["out1"].ViaEdge.Direction != "outgoing" || byID["out1"].ViaEdge.EdgeID != "e-out" {
		t.Fatalf("unexpected via_edge for out1: %+v", byID["out1"].ViaEdge)
	}
	if byID["in1"].ViaEdge.Direction != "incoming" || byID["in1"].ViaEdge.EdgeID != "e-in" {
		t.Fatalf("unexpected via_edge for in1: %+v", byID["in1"].ViaEdge)
	}
	if res.Truncated {
		t.Fatal("expected truncated=false")
	}
}

func TestNeighborsDepthTraversal(t *testing.T) {
	g, _ := newTestGraph(t)
	// chain: a -> b -> c -> d
	g.Write([]*Node{
		{NodeID: "a", Type: NodeTypeFinding, Summary: "a"},
		{NodeID: "b", Type: NodeTypeFinding, Summary: "b"},
		{NodeID: "c", Type: NodeTypeFinding, Summary: "c"},
		{NodeID: "d", Type: NodeTypeFinding, Summary: "d"},
	}, nil)
	g.Write(nil, []*Edge{
		{EdgeID: "e-ab", Type: EdgeTypeReferences, FromNodeID: "a", ToNodeID: "b"},
		{EdgeID: "e-bc", Type: EdgeTypeReferences, FromNodeID: "b", ToNodeID: "c"},
		{EdgeID: "e-cd", Type: EdgeTypeReferences, FromNodeID: "c", ToNodeID: "d"},
	})

	res, ok := g.Neighbors("a", 2, "outgoing", nil, 50)
	if !ok {
		t.Fatal("expected anchor to be found")
	}
	if len(res.Neighbors) != 2 {
		t.Fatalf("expected 2 neighbors at depth 2, got %d: %+v", len(res.Neighbors), res.Neighbors)
	}
	if res.Neighbors[0].NodeID != "b" || res.Neighbors[0].Hop != 1 {
		t.Fatalf("expected b at hop 1 first, got %+v", res.Neighbors[0])
	}
	if res.Neighbors[1].NodeID != "c" || res.Neighbors[1].Hop != 2 {
		t.Fatalf("expected c at hop 2 second, got %+v", res.Neighbors[1])
	}

	res3, ok := g.Neighbors("a", 3, "outgoing", nil, 50)
	if !ok {
		t.Fatal("expected anchor to be found")
	}
	if len(res3.Neighbors) != 3 {
		t.Fatalf("expected 3 neighbors at depth 3, got %d: %+v", len(res3.Neighbors), res3.Neighbors)
	}
}

func TestNeighborsDirectionFilter(t *testing.T) {
	g, _ := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "anchor", Type: NodeTypeFinding, Summary: "anchor"},
		{NodeID: "out1", Type: NodeTypeFinding, Summary: "out1"},
		{NodeID: "in1", Type: NodeTypeFinding, Summary: "in1"},
	}, nil)
	g.Write(nil, []*Edge{
		{EdgeID: "e-out", Type: EdgeTypeReferences, FromNodeID: "anchor", ToNodeID: "out1"},
		{EdgeID: "e-in", Type: EdgeTypeReferences, FromNodeID: "in1", ToNodeID: "anchor"},
	})

	outRes, _ := g.Neighbors("anchor", 1, "outgoing", nil, 50)
	if len(outRes.Neighbors) != 1 || outRes.Neighbors[0].NodeID != "out1" {
		t.Fatalf("expected only out1 for outgoing, got %+v", outRes.Neighbors)
	}

	inRes, _ := g.Neighbors("anchor", 1, "incoming", nil, 50)
	if len(inRes.Neighbors) != 1 || inRes.Neighbors[0].NodeID != "in1" {
		t.Fatalf("expected only in1 for incoming, got %+v", inRes.Neighbors)
	}
}

func TestNeighborsEdgeTypeFilter(t *testing.T) {
	g, _ := newTestGraph(t)
	g.Write([]*Node{
		{NodeID: "anchor", Type: NodeTypeFinding, Summary: "anchor"},
		{NodeID: "blocks-target", Type: NodeTypeFinding, Summary: "blocks-target"},
		{NodeID: "refs-target", Type: NodeTypeFinding, Summary: "refs-target"},
	}, nil)
	g.Write(nil, []*Edge{
		{EdgeID: "e-blocks", Type: EdgeTypeBlocks, FromNodeID: "anchor", ToNodeID: "blocks-target"},
		{EdgeID: "e-refs", Type: EdgeTypeReferences, FromNodeID: "anchor", ToNodeID: "refs-target"},
	})

	res, _ := g.Neighbors("anchor", 1, "both", []EdgeType{EdgeTypeBlocks}, 50)
	if len(res.Neighbors) != 1 || res.Neighbors[0].NodeID != "blocks-target" {
		t.Fatalf("expected only blocks-target for edge_types=[blocks], got %+v", res.Neighbors)
	}

	all, _ := g.Neighbors("anchor", 1, "both", nil, 50)
	if len(all.Neighbors) != 2 {
		t.Fatalf("expected both neighbors with no edge_types filter, got %+v", all.Neighbors)
	}
}

func TestNeighborsDedupShortestHopWins(t *testing.T) {
	g, _ := newTestGraph(t)
	// diamond: a -> b -> d, a -> c -> d (d reachable at hop 2 via two paths)
	// and a direct a -> d edge so d is also reachable at hop 1.
	g.Write([]*Node{
		{NodeID: "a", Type: NodeTypeFinding, Summary: "a"},
		{NodeID: "b", Type: NodeTypeFinding, Summary: "b"},
		{NodeID: "c", Type: NodeTypeFinding, Summary: "c"},
		{NodeID: "d", Type: NodeTypeFinding, Summary: "d"},
	}, nil)
	g.Write(nil, []*Edge{
		{EdgeID: "e-ad", Type: EdgeTypeReferences, FromNodeID: "a", ToNodeID: "d"},
		{EdgeID: "e-ab", Type: EdgeTypeReferences, FromNodeID: "a", ToNodeID: "b"},
		{EdgeID: "e-ac", Type: EdgeTypeReferences, FromNodeID: "a", ToNodeID: "c"},
		{EdgeID: "e-bd", Type: EdgeTypeReferences, FromNodeID: "b", ToNodeID: "d"},
		{EdgeID: "e-cd", Type: EdgeTypeReferences, FromNodeID: "c", ToNodeID: "d"},
	})

	res, _ := g.Neighbors("a", 2, "outgoing", nil, 50)
	dCount := 0
	var dHop int
	for _, n := range res.Neighbors {
		if n.NodeID == "d" {
			dCount++
			dHop = n.Hop
		}
	}
	if dCount != 1 {
		t.Fatalf("expected d to appear exactly once (dedup), got %d times", dCount)
	}
	if dHop != 1 {
		t.Fatalf("expected d at shortest hop 1, got hop %d", dHop)
	}
}

func TestNeighborsBoundedBFSTruncates(t *testing.T) {
	g, _ := newTestGraph(t)
	const fanout = 10
	nodes := []*Node{{NodeID: "anchor", Type: NodeTypeFinding, Summary: "anchor"}}
	edges := make([]*Edge, 0, fanout)
	for i := 0; i < fanout; i++ {
		id := fmt.Sprintf("leaf%d", i)
		nodes = append(nodes, &Node{NodeID: id, Type: NodeTypeFinding, Summary: id})
		edges = append(edges, &Edge{EdgeID: fmt.Sprintf("e%d", i), Type: EdgeTypeReferences, FromNodeID: "anchor", ToNodeID: id})
	}
	g.Write(nodes, nil)
	g.Write(nil, edges)

	res, ok := g.Neighbors("anchor", 1, "outgoing", nil, 3)
	if !ok {
		t.Fatal("expected anchor to be found")
	}
	if len(res.Neighbors) != 3 {
		t.Fatalf("expected exactly limit=3 neighbors, got %d", len(res.Neighbors))
	}
	if !res.Truncated {
		t.Fatal("expected truncated=true when reachable set exceeds limit")
	}

	full, _ := g.Neighbors("anchor", 1, "outgoing", nil, fanout)
	if full.Truncated {
		t.Fatal("expected truncated=false when limit exactly covers reachable set")
	}
	if len(full.Neighbors) != fanout {
		t.Fatalf("expected all %d neighbors, got %d", fanout, len(full.Neighbors))
	}
}

func TestNeighborsPerNodeEdgeCap(t *testing.T) {
	g, _ := newTestGraph(t)
	const hubDegree = maxEdgesPerNodeHop + 50
	nodes := []*Node{{NodeID: "hub", Type: NodeTypeFinding, Summary: "hub"}}
	edges := make([]*Edge, 0, hubDegree)
	for i := 0; i < hubDegree; i++ {
		id := fmt.Sprintf("leaf%d", i)
		nodes = append(nodes, &Node{NodeID: id, Type: NodeTypeFinding, Summary: id})
		edges = append(edges, &Edge{EdgeID: fmt.Sprintf("e%d", i), Type: EdgeTypeReferences, FromNodeID: "hub", ToNodeID: id})
	}
	g.Write(nodes, nil)
	g.Write(nil, edges)

	// A limit well above the per-node cap should still be bounded by the cap,
	// not by the caller's limit.
	res, ok := g.Neighbors("hub", 1, "outgoing", nil, hubDegree)
	if !ok {
		t.Fatal("expected hub to be found")
	}
	if len(res.Neighbors) != maxEdgesPerNodeHop {
		t.Fatalf("expected per-node edge cap of %d, got %d", maxEdgesPerNodeHop, len(res.Neighbors))
	}
	// The cap should keep the most-recent edges (tail of the adjacency slice).
	for _, n := range res.Neighbors {
		if n.NodeID == "leaf0" {
			t.Fatal("expected earliest edge (leaf0) to be dropped by the most-recent-N cap")
		}
	}
}

func TestNeighborsAnchorNotFound(t *testing.T) {
	g, _ := newTestGraph(t)
	if _, ok := g.Neighbors("missing", 1, "both", nil, 50); ok {
		t.Fatal("expected ok=false for missing anchor")
	}
}

func BenchmarkNeighborsChain(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const n = 10000
	nodes := make([]*Node, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, &Node{NodeID: fmt.Sprintf("n%d", i), Type: NodeTypeFinding, Summary: "x"})
	}
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write nodes: %v", err)
	}

	edges := make([]*Edge, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, &Edge{
			EdgeID:     fmt.Sprintf("e%d", i),
			Type:       EdgeTypeReferences,
			FromNodeID: fmt.Sprintf("n%d", i),
			ToNodeID:   fmt.Sprintf("n%d", i+1),
		})
	}
	if _, err := g.Write(nil, edges); err != nil {
		b.Fatalf("Write edges: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Neighbors("n5000", 2, "both", nil, 50)
	}
}

// BenchmarkNeighborsHub measures traversal cost from a single degenerate hub
// node, confirming the per-node edge cap bounds cost independent of degree.
func BenchmarkNeighborsHub(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const hubDegree = 10000
	nodes := make([]*Node, 0, hubDegree+1)
	nodes = append(nodes, &Node{NodeID: "hub", Type: NodeTypeFinding, Summary: "x"})
	edges := make([]*Edge, 0, hubDegree)
	for i := 0; i < hubDegree; i++ {
		id := fmt.Sprintf("leaf%d", i)
		nodes = append(nodes, &Node{NodeID: id, Type: NodeTypeFinding, Summary: "x"})
		edges = append(edges, &Edge{EdgeID: fmt.Sprintf("e%d", i), Type: EdgeTypeReferences, FromNodeID: "hub", ToNodeID: id})
	}
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write nodes: %v", err)
	}
	if _, err := g.Write(nil, edges); err != nil {
		b.Fatalf("Write edges: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Neighbors("hub", 1, "outgoing", nil, 50)
	}
}

func BenchmarkNodesSinceRareTypeFilter(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	// 50k findings, 1 decision buried at the very end — worst case for a
	// linear scan filtering down to a rare type.
	nodes := make([]*Node, 0, 50001)
	for i := 0; i < 50000; i++ {
		nodes = append(nodes, &Node{NodeID: fmt.Sprintf("f%d", i), Type: NodeTypeFinding, Summary: "x"})
	}
	nodes = append(nodes, &Node{NodeID: "rare-decision", Type: NodeTypeDecision, Summary: "x"})
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.NodesSince(0, 10, []NodeType{NodeTypeDecision})
	}
}

func BenchmarkWriteBatch(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Write([]*Node{{NodeID: fmt.Sprintf("n%d", i), Type: NodeTypeFinding, Summary: "x"}}, nil)
	}
}

func BenchmarkGetNode(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const n = 10000
	nodes := make([]*Node, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, &Node{NodeID: fmt.Sprintf("n%d", i), Type: NodeTypeFinding, Summary: "x"})
	}
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write nodes: %v", err)
	}

	// Chain edges so a mid-chain node has both an incoming and an outgoing
	// edge (deg(v)=2) — a representative small-degree lookup.
	edges := make([]*Edge, 0, n-1)
	for i := 0; i < n-1; i++ {
		edges = append(edges, &Edge{
			EdgeID:     fmt.Sprintf("e%d", i),
			Type:       EdgeTypeReferences,
			FromNodeID: fmt.Sprintf("n%d", i),
			ToNodeID:   fmt.Sprintf("n%d", i+1),
		})
	}
	if _, err := g.Write(nil, edges); err != nil {
		b.Fatalf("Write edges: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.GetNode("n5000")
	}
}

func BenchmarkNodesSinceWildcard(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const n = 50000
	nodes := make([]*Node, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, &Node{NodeID: fmt.Sprintf("f%d", i), Type: NodeTypeFinding, Summary: "x"})
	}
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.NodesSince(0, 10, nil)
	}
}

func BenchmarkNodesSinceMultiType(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const perType = 10000
	types := []NodeType{NodeTypeFinding, NodeTypeDecision, NodeTypeBlocker}
	nodes := make([]*Node, 0, perType*len(types))
	for _, t := range types {
		for i := 0; i < perType; i++ {
			nodes = append(nodes, &Node{NodeID: fmt.Sprintf("%s-%d", t, i), Type: t, Summary: "x"})
		}
	}
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.NodesSince(0, 100, types)
	}
}

func BenchmarkSnapshot(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const n = 10000
	nodes := make([]*Node, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, &Node{NodeID: fmt.Sprintf("n%d", i), Type: NodeTypeFinding, Summary: "x"})
	}
	if _, err := g.Write(nodes, nil); err != nil {
		b.Fatalf("Write nodes: %v", err)
	}
	edges := make([]*Edge, 0, n/2)
	for i := 0; i < n/2; i++ {
		edges = append(edges, &Edge{
			EdgeID:     fmt.Sprintf("e%d", i),
			Type:       EdgeTypeReferences,
			FromNodeID: fmt.Sprintf("n%d", i),
			ToNodeID:   fmt.Sprintf("n%d", i+1),
		})
	}
	if _, err := g.Write(nil, edges); err != nil {
		b.Fatalf("Write edges: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Snapshot()
	}
}

// BenchmarkTopicLoad measures cold-start cost: opening a topic backed by an
// existing, already-populated WAL file (the O(L) path).
func BenchmarkTopicLoad(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	seed, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}

	const n = 20000
	nodes := make([]*Node, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, &Node{NodeID: fmt.Sprintf("n%d", i), Type: NodeTypeFinding, Summary: "x"})
	}
	if _, err := seed.Write(nodes, nil); err != nil {
		b.Fatalf("Write: %v", err)
	}
	seed.Close() // ensure the WAL is fully flushed to disk before timing loads

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g, err := newTopicGraph(path)
		if err != nil {
			b.Fatalf("newTopicGraph: %v", err)
		}
		b.StopTimer()
		g.Close()
		b.StartTimer()
	}
}

func BenchmarkWriteBatchLarge(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	const batchSize = 50
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nodes := make([]*Node, batchSize)
		for j := 0; j < batchSize; j++ {
			nodes[j] = &Node{NodeID: fmt.Sprintf("n%d-%d", i, j), Type: NodeTypeFinding, Summary: "x"}
		}
		if _, err := g.Write(nodes, nil); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}

// BenchmarkWriteParallel measures write throughput/lock contention under
// concurrent callers, matching the shape of TestConcurrentWritesRemainConsistent.
func BenchmarkWriteParallel(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		b.Fatalf("newTopicGraph: %v", err)
	}
	defer g.Close()

	var counter int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := atomic.AddInt64(&counter, 1)
			g.Write([]*Node{{NodeID: fmt.Sprintf("n%d", id), Type: NodeTypeFinding, Summary: "x"}}, nil)
		}
	})
}
