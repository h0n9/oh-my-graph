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
