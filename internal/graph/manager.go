package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var topicNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Manager struct {
	mu      sync.RWMutex
	baseDir string
	graphs  map[string]*TopicGraph
}

func NewManager(baseDir string) *Manager {
	os.MkdirAll(baseDir, 0755)
	return &Manager{
		baseDir: baseDir,
		graphs:  make(map[string]*TopicGraph),
	}
}

func (m *Manager) Topics() []string {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return []string{}
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && topicNameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

func (m *Manager) Topic(name string) (*TopicGraph, error) {
	if !topicNameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid topic name %q: must match [a-zA-Z0-9_-]{1,64}", name)
	}

	m.mu.RLock()
	g, ok := m.graphs[name]
	m.mu.RUnlock()
	if ok {
		return g, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if g, ok := m.graphs[name]; ok {
		return g, nil
	}

	dir := filepath.Join(m.baseDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create topic directory: %w", err)
	}

	path := filepath.Join(dir, "graph.jsonl")
	g, err := newTopicGraph(path)
	if err != nil {
		return nil, fmt.Errorf("load topic %q: %w", name, err)
	}

	m.graphs[name] = g
	return g, nil
}

func (m *Manager) Close() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.graphs {
		g.Close()
	}
}
