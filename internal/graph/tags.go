package graph

import (
	"container/heap"
	"sort"
)

type tagCountHeap []TagCount

func (h tagCountHeap) Len() int { return len(h) }

// The root is the worst retained item: lowest count, then largest tag.
func (h tagCountHeap) Less(i, j int) bool {
	if h[i].NodeCount != h[j].NodeCount {
		return h[i].NodeCount < h[j].NodeCount
	}
	return h[i].Tag > h[j].Tag
}
func (h tagCountHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *tagCountHeap) Push(x any)   { *h = append(*h, x.(TagCount)) }
func (h *tagCountHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func betterTagCount(a, b TagCount) bool {
	return a.NodeCount > b.NodeCount || (a.NodeCount == b.NodeCount && a.Tag < b.Tag)
}

func taggedSummary(g *TopicGraph, entry nodeLogEntry) TaggedNodeSummary {
	n := g.nodes[entry.nodeID]
	return TaggedNodeSummary{
		NodeID: n.NodeID, Type: n.Type, Summary: n.Summary, Seq: entry.seq,
		Tags: append([]string(nil), n.Tags...),
	}
}

func nodeTypeAllowed(t NodeType, types map[NodeType]struct{}) bool {
	if len(types) == 0 {
		return true
	}
	_, ok := types[t]
	return ok
}

func hasAllTags(nodeTags []string, requested []string) bool {
	for _, requestedTag := range requested {
		found := false
		for _, nodeTag := range nodeTags {
			if nodeTag == requestedTag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SearchByTags performs exact tag matching and returns newest nodes first.
// For "all", it walks only the smallest posting list. For "any", it merges
// the append-ordered posting lists and deduplicates nodes carrying several tags.
func (g *TopicGraph) SearchByTags(tags []string, match string, types []NodeType, limit int) []TaggedNodeSummary {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := make([]TaggedNodeSummary, 0)
	typeSet := make(map[NodeType]struct{}, len(types))
	for _, t := range types {
		typeSet[t] = struct{}{}
	}

	if match == "all" {
		postings := g.tagLog[tags[0]]
		for _, tag := range tags[1:] {
			if len(g.tagLog[tag]) < len(postings) {
				postings = g.tagLog[tag]
			}
		}
		for i := len(postings) - 1; i >= 0 && len(result) < limit; i-- {
			n := g.nodes[postings[i].nodeID]
			if nodeTypeAllowed(n.Type, typeSet) && hasAllTags(n.Tags, tags) {
				result = append(result, taggedSummary(g, postings[i]))
			}
		}
		return result
	}

	indices := make([]int, len(tags))
	for i, tag := range tags {
		indices[i] = len(g.tagLog[tag]) - 1
	}
	seen := make(map[string]struct{})
	for len(result) < limit {
		best := -1
		for i, tag := range tags {
			if indices[i] < 0 {
				continue
			}
			entry := g.tagLog[tag][indices[i]]
			if best == -1 {
				best = i
				continue
			}
			bestEntry := g.tagLog[tags[best]][indices[best]]
			if entry.seq > bestEntry.seq || (entry.seq == bestEntry.seq && entry.nodeID < bestEntry.nodeID) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		entry := g.tagLog[tags[best]][indices[best]]
		indices[best]--
		if _, exists := seen[entry.nodeID]; exists {
			continue
		}
		seen[entry.nodeID] = struct{}{}
		n := g.nodes[entry.nodeID]
		if nodeTypeAllowed(n.Type, typeSet) {
			result = append(result, taggedSummary(g, entry))
		}
	}
	return result
}

// ListTags returns the most-used tags, with stable lexical tie-breaking.
func (g *TopicGraph) ListTags(limit int) []TagCount {
	g.mu.RLock()
	defer g.mu.RUnlock()

	counts := make(tagCountHeap, 0, min(limit, len(g.tagLog)))
	for tag, entries := range g.tagLog {
		candidate := TagCount{Tag: tag, NodeCount: len(entries)}
		if len(counts) < limit {
			heap.Push(&counts, candidate)
		} else if betterTagCount(candidate, counts[0]) {
			counts[0] = candidate
			heap.Fix(&counts, 0)
		}
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].NodeCount != counts[j].NodeCount {
			return counts[i].NodeCount > counts[j].NodeCount
		}
		return counts[i].Tag < counts[j].Tag
	})
	return []TagCount(counts)
}
