package graph

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

var validNodeTypes = map[NodeType]bool{
	NodeTypeFinding:  true,
	NodeTypeConcept:  true,
	NodeTypeBlocker:  true,
	NodeTypeQuestion: true,
	NodeTypeDecision: true,
	NodeTypeArtifact: true,
	NodeTypeEntity:   true,
	NodeTypeEvent:    true,
	NodeTypeMessage:  true,
}

var validEdgeTypes = map[EdgeType]bool{
	EdgeTypeResolves:    true,
	EdgeTypeProduces:    true,
	EdgeTypeBlocks:      true,
	EdgeTypeCauses:      true,
	EdgeTypeSupports:    true,
	EdgeTypeContradicts: true,
	EdgeTypeDependsOn:   true,
	EdgeTypePartOf:      true,
	EdgeTypeReferences:  true,
	EdgeTypeRepliesTo:   true,
	EdgeTypeDeprecates:  true,
}

type nodeLogEntry struct {
	seq    int64
	nodeID string
}

type TopicGraph struct {
	mu     sync.RWMutex
	fileMu sync.Mutex
	wg     sync.WaitGroup

	path    string
	nodes   map[string]*Node
	edges   map[string]*Edge
	adjOut  map[string][]*Edge
	adjIn   map[string][]*Edge
	nodeLog []nodeLogEntry
	headSeq int64
}

func newTopicGraph(path string) (*TopicGraph, error) {
	g := &TopicGraph{
		path:   path,
		nodes:  make(map[string]*Node),
		edges:  make(map[string]*Edge),
		adjOut: make(map[string][]*Edge),
		adjIn:  make(map[string][]*Edge),
	}
	if err := g.load(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *TopicGraph) load() error {
	f, err := os.Open(g.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var rec WALRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("parse WAL record at seq %d: %w", rec.Seq, err)
		}

		switch rec.Type {
		case "node":
			n := new(Node)
			if err := json.Unmarshal(rec.Data, n); err != nil {
				return fmt.Errorf("parse node data: %w", err)
			}
			g.nodes[n.NodeID] = n
			g.nodeLog = append(g.nodeLog, nodeLogEntry{seq: rec.Seq, nodeID: n.NodeID})
		case "edge":
			e := new(Edge)
			if err := json.Unmarshal(rec.Data, e); err != nil {
				return fmt.Errorf("parse edge data: %w", err)
			}
			g.edges[e.EdgeID] = e
			g.adjOut[e.FromNodeID] = append(g.adjOut[e.FromNodeID], e)
			g.adjIn[e.ToNodeID] = append(g.adjIn[e.ToNodeID], e)
		}

		if rec.Seq > g.headSeq {
			g.headSeq = rec.Seq
		}
	}

	return scanner.Err()
}

func (g *TopicGraph) Write(nodes []*Node, edges []*Edge) (int64, error) {
	if len(nodes) == 0 && len(edges) == 0 {
		g.mu.RLock()
		h := g.headSeq
		g.mu.RUnlock()
		return h, nil
	}

	g.mu.Lock()

	// build set of node IDs available after this write (existing + batch)
	batchNodeIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		batchNodeIDs[n.NodeID] = true
	}

	for _, n := range nodes {
		if n.NodeID == "" {
			g.mu.Unlock()
			return 0, fmt.Errorf("node missing node_id")
		}
		if !validNodeTypes[n.Type] {
			g.mu.Unlock()
			return 0, fmt.Errorf("invalid node type: %q", n.Type)
		}
		if n.Confidence < 0 || n.Confidence > 1 {
			g.mu.Unlock()
			return 0, fmt.Errorf("node %s: confidence must be between 0 and 1", n.NodeID)
		}
		if _, exists := g.nodes[n.NodeID]; exists {
			g.mu.Unlock()
			return 0, fmt.Errorf("node %s already exists", n.NodeID)
		}
	}

	for _, e := range edges {
		if e.EdgeID == "" {
			g.mu.Unlock()
			return 0, fmt.Errorf("edge missing edge_id")
		}
		if !validEdgeTypes[e.Type] {
			g.mu.Unlock()
			return 0, fmt.Errorf("invalid edge type: %q", e.Type)
		}
		if _, ok := g.nodes[e.FromNodeID]; !ok {
			if !batchNodeIDs[e.FromNodeID] {
				g.mu.Unlock()
				return 0, fmt.Errorf("edge %s: from_node_id %s not found", e.EdgeID, e.FromNodeID)
			}
		}
		if _, ok := g.nodes[e.ToNodeID]; !ok {
			if !batchNodeIDs[e.ToNodeID] {
				g.mu.Unlock()
				return 0, fmt.Errorf("edge %s: to_node_id %s not found", e.EdgeID, e.ToNodeID)
			}
		}
		if _, exists := g.edges[e.EdgeID]; exists {
			g.mu.Unlock()
			return 0, fmt.Errorf("edge %s already exists", e.EdgeID)
		}
	}

	now := time.Now().UTC()
	records := make([]WALRecord, 0, len(nodes)+len(edges))

	for _, n := range nodes {
		g.headSeq++
		data, _ := json.Marshal(n)
		records = append(records, WALRecord{
			Seq:  g.headSeq,
			Type: "node",
			TS:   now,
			Data: json.RawMessage(data),
		})
		g.nodes[n.NodeID] = n
		g.nodeLog = append(g.nodeLog, nodeLogEntry{seq: g.headSeq, nodeID: n.NodeID})
	}

	for _, e := range edges {
		g.headSeq++
		data, _ := json.Marshal(e)
		records = append(records, WALRecord{
			Seq:  g.headSeq,
			Type: "edge",
			TS:   now,
			Data: json.RawMessage(data),
		})
		g.edges[e.EdgeID] = e
		g.adjOut[e.FromNodeID] = append(g.adjOut[e.FromNodeID], e)
		g.adjIn[e.ToNodeID] = append(g.adjIn[e.ToNodeID], e)
	}

	newHead := g.headSeq
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.fileMu.Lock()
		defer g.fileMu.Unlock()
		if err := g.appendToFile(records); err != nil {
			log.Printf("oh-my-graph: failed to append to %s: %v", g.path, err)
		}
	}()

	return newHead, nil
}

func (g *TopicGraph) appendToFile(records []WALRecord) error {
	f, err := os.OpenFile(g.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return w.Flush()
}

func (g *TopicGraph) NodesSince(cursor int64, limit int) []NodeSummary {
	g.mu.RLock()
	defer g.mu.RUnlock()

	i := sort.Search(len(g.nodeLog), func(i int) bool {
		return g.nodeLog[i].seq > cursor
	})

	slice := g.nodeLog[i:]
	if limit > 0 && limit < len(slice) {
		slice = slice[:limit]
	}
	result := make([]NodeSummary, 0, len(slice))
	for _, entry := range slice {
		n := g.nodes[entry.nodeID]
		result = append(result, NodeSummary{
			NodeID:  n.NodeID,
			Summary: n.Summary,
			Seq:     entry.seq,
		})
	}
	return result
}

func (g *TopicGraph) GetNode(nodeID string) (*NodeWithEdges, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	n, ok := g.nodes[nodeID]
	if !ok {
		return nil, false
	}

	edges := make([]*Edge, 0, len(g.adjOut[nodeID])+len(g.adjIn[nodeID]))
	edges = append(edges, g.adjOut[nodeID]...)
	edges = append(edges, g.adjIn[nodeID]...)

	return &NodeWithEdges{Node: n, Edges: edges}, true
}

func (g *TopicGraph) Stats() (headSeq int64, nodeCount, edgeCount int) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.headSeq, len(g.nodes), len(g.edges)
}

func (g *TopicGraph) Snapshot() ([]*Node, []*Edge) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		nodes = append(nodes, n)
	}

	edges := make([]*Edge, 0, len(g.edges))
	for _, e := range g.edges {
		edges = append(edges, e)
	}

	return nodes, edges
}

func (g *TopicGraph) Close() {
	g.wg.Wait()
}
