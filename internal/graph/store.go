package graph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
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
	// typeLog partitions nodeLog by NodeType so type-filtered reads (the common
	// case for read_nodes_since, which defaults to type=finding) don't need to
	// linear-scan past every other type to find matches.
	typeLog map[NodeType][]nodeLogEntry
	headSeq int64
	// readOffset is the byte offset up to which the WAL file has been fully
	// parsed into memory. It lets reloadNewLines resume from where it left off
	// instead of rescanning the entire file on every change notification.
	readOffset int64
	walFile    *os.File
	watcher    *fsnotify.Watcher
}

func newTopicGraph(path string) (*TopicGraph, error) {
	g := &TopicGraph{
		path:    path,
		nodes:   make(map[string]*Node),
		edges:   make(map[string]*Edge),
		adjOut:  make(map[string][]*Edge),
		adjIn:   make(map[string][]*Edge),
		typeLog: make(map[NodeType][]nodeLogEntry),
	}
	if err := g.load(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open WAL for append: %w", err)
	}
	g.walFile = f

	if err := g.startWatcher(); err != nil {
		log.Printf("oh-my-graph: file watcher unavailable for %s: %v", path, err)
	} else {
		log.Printf("oh-my-graph: watching %s for external changes", path)
	}
	return g, nil
}

// indexNode records n under both the seq-ordered log and its type's log. Callers
// must hold g.mu (write lock).
func (g *TopicGraph) indexNode(seq int64, n *Node) {
	entry := nodeLogEntry{seq: seq, nodeID: n.NodeID}
	g.nodeLog = append(g.nodeLog, entry)
	g.typeLog[n.Type] = append(g.typeLog[n.Type], entry)
}

// applyRecord applies a single WAL record to in-memory state. It is idempotent:
// a record whose node/edge ID already exists is skipped, so callers can safely
// re-apply records without first checking whether they've been seen. Callers
// must hold g.mu (write lock).
func (g *TopicGraph) applyRecord(rec WALRecord) {
	switch rec.Type {
	case "node":
		n := new(Node)
		if err := json.Unmarshal(rec.Data, n); err != nil {
			return
		}
		if _, exists := g.nodes[n.NodeID]; !exists {
			g.nodes[n.NodeID] = n
			g.indexNode(rec.Seq, n)
		}
	case "edge":
		e := new(Edge)
		if err := json.Unmarshal(rec.Data, e); err != nil {
			return
		}
		if _, exists := g.edges[e.EdgeID]; !exists {
			g.edges[e.EdgeID] = e
			g.adjOut[e.FromNodeID] = append(g.adjOut[e.FromNodeID], e)
			g.adjIn[e.ToNodeID] = append(g.adjIn[e.ToNodeID], e)
		}
	}
	if rec.Seq > g.headSeq {
		g.headSeq = rec.Seq
	}
}

// scanFrom reads the WAL file starting at byte offset `from`, applying every
// fully newline-terminated record it finds. It returns the offset up to which
// it successfully consumed complete lines. A trailing line with no terminating
// '\n' (e.g. a concurrent writer caught mid-flush) is left unconsumed so it is
// safely re-read, complete, on the next call — this keeps incremental reads
// correct even when scanFrom races an in-progress append.
func (g *TopicGraph) scanFrom(from int64) (int64, error) {
	f, err := os.Open(g.path)
	if os.IsNotExist(err) {
		return from, nil
	}
	if err != nil {
		return from, err
	}
	defer f.Close()

	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return from, err
		}
	}

	r := bufio.NewReaderSize(f, 1024*1024)
	pos := from
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 && readErr == nil {
			pos += int64(len(line))
			trimmed := bytes.TrimRight(line, "\n")
			if len(trimmed) > 0 {
				var rec WALRecord
				if err := json.Unmarshal(trimmed, &rec); err != nil {
					return pos, fmt.Errorf("parse WAL record: %w", err)
				}
				g.applyRecord(rec)
			}
		}
		if readErr != nil {
			break
		}
	}
	return pos, nil
}

func (g *TopicGraph) load() error {
	pos, err := g.scanFrom(0)
	if err != nil {
		return err
	}
	g.readOffset = pos
	return nil
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
		g.indexNode(g.headSeq, n)
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
		// fileMu is held across both the write and the readOffset bump below
		// (not released in between) so a concurrent reload can never observe
		// these bytes and advance readOffset for them independently — that
		// would double-count the same bytes once here and once there,
		// desyncing readOffset from the actual file layout. See startWatcher,
		// which acquires fileMu before mu for the same reason.
		g.fileMu.Lock()
		defer g.fileMu.Unlock()
		n, err := g.appendToFile(records)
		if err != nil {
			log.Printf("oh-my-graph: failed to append to %s: %v", g.path, err)
			return
		}
		// Advance readOffset by exactly what we just wrote, so the fsnotify
		// watcher's reload sees nothing new to do for our own writes instead
		// of re-scanning and re-parsing bytes we already hold in memory.
		g.mu.Lock()
		g.readOffset += n
		g.mu.Unlock()
	}()

	return newHead, nil
}

// appendToFile writes records to the topic's persistent WAL file handle in a
// single write call and returns the number of bytes written.
func (g *TopicGraph) appendToFile(records []WALRecord) (int64, error) {
	var buf bytes.Buffer
	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			return 0, err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	n, err := g.walFile.Write(buf.Bytes())
	return int64(n), err
}

// NodesSince returns node summaries with seq > cursor, optionally filtered by
// type, in ascending seq order, capped at limit. An empty types list matches
// every type. A single type is served directly from that type's own log
// (O(log N) to the cursor, O(k) to collect); multiple types are merged across
// their individual logs in seq order.
func (g *TopicGraph) NodesSince(cursor int64, limit int, types []NodeType) []NodeSummary {
	g.mu.RLock()
	defer g.mu.RUnlock()

	switch len(types) {
	case 0:
		return g.sliceFrom(g.nodeLog, cursor, limit)
	case 1:
		return g.sliceFrom(g.typeLog[types[0]], cursor, limit)
	default:
		return g.mergeTypeLogs(types, cursor, limit)
	}
}

func (g *TopicGraph) sliceFrom(log []nodeLogEntry, cursor int64, limit int) []NodeSummary {
	i := sort.Search(len(log), func(i int) bool { return log[i].seq > cursor })
	remaining := log[i:]

	capHint := len(remaining)
	if limit > 0 && limit < capHint {
		capHint = limit
	}
	result := make([]NodeSummary, 0, capHint)
	for _, entry := range remaining {
		n := g.nodes[entry.nodeID]
		result = append(result, NodeSummary{
			NodeID:  n.NodeID,
			Type:    n.Type,
			Summary: n.Summary,
			Seq:     entry.seq,
		})
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

// mergeTypeLogs k-way merges the per-type logs for the requested types,
// starting each from its own cursor position, in ascending seq order.
func (g *TopicGraph) mergeTypeLogs(types []NodeType, cursor int64, limit int) []NodeSummary {
	logs := make([][]nodeLogEntry, len(types))
	idx := make([]int, len(types))
	for i, t := range types {
		l := g.typeLog[t]
		logs[i] = l
		idx[i] = sort.Search(len(l), func(j int) bool { return l[j].seq > cursor })
	}

	result := make([]NodeSummary, 0)
	for {
		best := -1
		for i := range logs {
			if idx[i] >= len(logs[i]) {
				continue
			}
			if best == -1 || logs[i][idx[i]].seq < logs[best][idx[best]].seq {
				best = i
			}
		}
		if best == -1 {
			break
		}
		entry := logs[best][idx[best]]
		idx[best]++

		n := g.nodes[entry.nodeID]
		result = append(result, NodeSummary{
			NodeID:  n.NodeID,
			Type:    n.Type,
			Summary: n.Summary,
			Seq:     entry.seq,
		})
		if limit > 0 && len(result) >= limit {
			break
		}
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

func (g *TopicGraph) startWatcher() error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	// Ensure the file exists before watching — fsnotify requires the path to exist.
	if _, err := os.Stat(g.path); os.IsNotExist(err) {
		if f, err := os.Create(g.path); err == nil {
			f.Close()
		}
	}
	if err := w.Add(g.path); err != nil {
		w.Close()
		return err
	}
	g.watcher = w
	go func() {
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					// Acquire fileMu before mu, matching Write()'s goroutine,
					// so a reload and an in-flight write's readOffset bump
					// can never interleave and double-count the same bytes.
					g.fileMu.Lock()
					g.mu.Lock()
					g.reloadNewLines()
					g.mu.Unlock()
					g.fileMu.Unlock()
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return nil
}

// reloadNewLines must be called under both g.fileMu.Lock() and g.mu.Lock()
// (fileMu acquired first — see startWatcher and Write). Holding fileMu is
// what prevents it from racing a concurrent write's own readOffset bump. It
// resumes scanning from g.readOffset rather than the start of the file, so
// both genuine external changes and this process's own writes (which
// self-trigger the watcher) cost O(bytes since last scan) instead of
// O(entire file history).
func (g *TopicGraph) reloadNewLines() {
	fi, err := os.Stat(g.path)
	if err != nil {
		return
	}
	if fi.Size() < g.readOffset {
		// File shrank out from under us (truncated/rotated externally) — the
		// offset we were tracking is no longer valid, so rebuild from scratch.
		log.Printf("oh-my-graph: %s shrank unexpectedly, reloading from scratch", g.path)
		g.nodes = make(map[string]*Node)
		g.edges = make(map[string]*Edge)
		g.adjOut = make(map[string][]*Edge)
		g.adjIn = make(map[string][]*Edge)
		g.nodeLog = nil
		g.typeLog = make(map[NodeType][]nodeLogEntry)
		g.headSeq = 0
		g.readOffset = 0
	}

	pos, err := g.scanFrom(g.readOffset)
	if err != nil {
		log.Printf("oh-my-graph: failed to reload %s: %v", g.path, err)
		return
	}
	if pos == g.readOffset {
		return
	}
	g.readOffset = pos
	log.Printf("oh-my-graph: reloaded %s, head seq now %d", g.path, g.headSeq)
}

func (g *TopicGraph) Close() {
	if g.watcher != nil {
		g.watcher.Close()
	}
	g.wg.Wait()
	if g.walFile != nil {
		g.walFile.Close()
	}
}
