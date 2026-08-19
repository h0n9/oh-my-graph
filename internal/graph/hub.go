package graph

import (
	"container/heap"
	"sort"
)

type hubSnapshotNode struct {
	nodeID     string
	typeName   NodeType
	summary    string
	seq        int64
	confidence *float64
}

type nodePair struct {
	a string
	b string
}

type hubSnapshot struct {
	nodes []hubSnapshotNode
	edges []nodePair
}

func copyFloat64(v *float64) *float64 {
	if v == nil {
		return nil
	}
	copied := *v
	return &copied
}

// hubSnapshot copies only the immutable values needed for hub ranking. The
// graph lock is released before topology construction, deduplication, and heap
// operations so those costs do not block writers.
func (g *TopicGraph) hubSnapshot() hubSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()

	snapshot := hubSnapshot{
		nodes: make([]hubSnapshotNode, 0, len(g.nodes)),
		edges: make([]nodePair, 0, len(g.edges)),
	}
	for _, n := range g.nodes {
		snapshot.nodes = append(snapshot.nodes, hubSnapshotNode{
			nodeID:     n.NodeID,
			typeName:   n.Type,
			summary:    n.Summary,
			seq:        g.nodeSeq[n.NodeID],
			confidence: copyFloat64(n.Confidence),
		})
	}
	for _, e := range g.edges {
		snapshot.edges = append(snapshot.edges, nodePair{a: e.FromNodeID, b: e.ToNodeID})
	}
	return snapshot
}

// betterHub reports whether a must appear before b in the public result.
func betterHub(a, b HubNode) bool {
	if a.NeighborCount != b.NeighborCount {
		return a.NeighborCount > b.NeighborCount
	}
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	return a.NodeID < b.NodeID
}

// hubMinHeap keeps the worst selected candidate at index zero.
type hubMinHeap []HubNode

func (h hubMinHeap) Len() int { return len(h) }
func (h hubMinHeap) Less(i, j int) bool {
	return betterHub(h[j], h[i])
}
func (h hubMinHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *hubMinHeap) Push(x any)   { *h = append(*h, x.(HubNode)) }
func (h *hubMinHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func rankHubNodes(snapshot hubSnapshot, limit int, types []NodeType) []HubNode {
	result := make([]HubNode, 0, limit)
	if limit <= 0 || len(snapshot.nodes) == 0 {
		return result
	}

	nodes := make(map[string]hubSnapshotNode, len(snapshot.nodes))
	for _, n := range snapshot.nodes {
		nodes[n.nodeID] = n
	}

	uniquePairs := make(map[nodePair]struct{}, len(snapshot.edges))
	degrees := make(map[string]int, len(snapshot.nodes))
	for _, edge := range snapshot.edges {
		if edge.a == edge.b {
			continue
		}
		if _, ok := nodes[edge.a]; !ok {
			continue
		}
		if _, ok := nodes[edge.b]; !ok {
			continue
		}
		if edge.a > edge.b {
			edge.a, edge.b = edge.b, edge.a
		}
		if _, exists := uniquePairs[edge]; exists {
			continue
		}
		uniquePairs[edge] = struct{}{}
		degrees[edge.a]++
		degrees[edge.b]++
	}

	var typeFilter map[NodeType]struct{}
	if len(types) > 0 {
		typeFilter = make(map[NodeType]struct{}, len(types))
		for _, t := range types {
			typeFilter[t] = struct{}{}
		}
	}

	selected := make(hubMinHeap, 0, limit)
	heap.Init(&selected)
	for _, n := range snapshot.nodes {
		degree := degrees[n.nodeID]
		if degree == 0 {
			continue
		}
		if typeFilter != nil {
			if _, ok := typeFilter[n.typeName]; !ok {
				continue
			}
		}
		candidate := HubNode{
			NodeID:        n.nodeID,
			Type:          n.typeName,
			Summary:       n.summary,
			Seq:           n.seq,
			NeighborCount: degree,
			Confidence:    copyFloat64(n.confidence),
		}
		if selected.Len() < limit {
			heap.Push(&selected, candidate)
		} else if betterHub(candidate, selected[0]) {
			selected[0] = candidate
			heap.Fix(&selected, 0)
		}
	}

	result = append(result, selected...)
	sort.Slice(result, func(i, j int) bool { return betterHub(result[i], result[j]) })
	return result
}

// HubNodes returns the highest-connectivity eligible nodes. Connectivity is
// distinct undirected neighbor count across the entire topic; types filter
// result candidates only. Nodes without a neighbor are omitted.
func (g *TopicGraph) HubNodes(limit int, types []NodeType) []HubNode {
	return rankHubNodes(g.hubSnapshot(), limit, types)
}
