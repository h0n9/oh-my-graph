package graph

import (
	"math"
	"os"
	"sort"
	"testing"
)

const (
	hubEvalDamping    = 0.85
	hubEvalTolerance  = 1e-9
	hubEvalIterations = 100
)

// pageRankSnapshot is deliberately test-only evaluation code. Production hub
// ranking remains distinct-neighbor degree and exposes no strategy switch.
func pageRankSnapshot(snapshot hubSnapshot) map[string]float64 {
	nodes := make(map[string]struct{}, len(snapshot.nodes))
	for _, n := range snapshot.nodes {
		nodes[n.nodeID] = struct{}{}
	}
	neighbors := make(map[string]map[string]struct{}, len(nodes))
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
		if neighbors[edge.a] == nil {
			neighbors[edge.a] = make(map[string]struct{})
		}
		if neighbors[edge.b] == nil {
			neighbors[edge.b] = make(map[string]struct{})
		}
		neighbors[edge.a][edge.b] = struct{}{}
		neighbors[edge.b][edge.a] = struct{}{}
	}
	if len(nodes) == 0 {
		return map[string]float64{}
	}

	n := float64(len(nodes))
	ranks := make(map[string]float64, len(nodes))
	for id := range nodes {
		ranks[id] = 1 / n
	}
	for iteration := 0; iteration < hubEvalIterations; iteration++ {
		dangling := 0.0
		for id := range nodes {
			if len(neighbors[id]) == 0 {
				dangling += ranks[id]
			}
		}
		next := make(map[string]float64, len(nodes))
		maxDelta := 0.0
		for id := range nodes {
			score := (1-hubEvalDamping)/n + hubEvalDamping*dangling/n
			for source := range neighbors[id] {
				score += hubEvalDamping * ranks[source] / float64(len(neighbors[source]))
			}
			next[id] = score
			maxDelta = math.Max(maxDelta, math.Abs(score-ranks[id]))
		}
		ranks = next
		if maxDelta < hubEvalTolerance {
			break
		}
	}
	return ranks
}

func TestHubRankingEvaluation(t *testing.T) {
	if os.Getenv("OMG_HUB_EVAL") != "1" {
		t.Skip("set OMG_HUB_EVAL=1 to run the offline PageRank comparison")
	}

	snapshot := hubSnapshot{
		nodes: []hubSnapshotNode{
			{nodeID: "center", typeName: NodeTypeConcept, seq: 1},
			{nodeID: "a", typeName: NodeTypeFinding, seq: 2},
			{nodeID: "b", typeName: NodeTypeFinding, seq: 3},
			{nodeID: "c", typeName: NodeTypeFinding, seq: 4},
		},
		edges: []nodePair{{a: "center", b: "a"}, {a: "center", b: "b"}, {a: "center", b: "c"}},
	}
	degree := rankHubNodes(snapshot, len(snapshot.nodes), nil)
	ranks := pageRankSnapshot(snapshot)
	pageRankIDs := make([]string, 0, len(ranks))
	for id := range ranks {
		pageRankIDs = append(pageRankIDs, id)
	}
	sort.Slice(pageRankIDs, func(i, j int) bool {
		if ranks[pageRankIDs[i]] != ranks[pageRankIDs[j]] {
			return ranks[pageRankIDs[i]] > ranks[pageRankIDs[j]]
		}
		return pageRankIDs[i] < pageRankIDs[j]
	})
	if degree[0].NodeID != "center" || pageRankIDs[0] != "center" {
		t.Fatalf("unexpected star ranking: degree=%+v pagerank=%+v", degree, pageRankIDs)
	}
	t.Logf("degree order=%v PageRank order=%v PageRank=%v", hubIDs(degree), pageRankIDs, ranks)
}

func hubIDs(nodes []HubNode) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.NodeID
	}
	return ids
}
