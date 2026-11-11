package graph

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var topicNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

const deletionPrefix = ".oh-my-graph-deleting-"

var deletionDirRE = regexp.MustCompile(`^\.oh-my-graph-deleting-[0-9]+$`)

var (
	ErrInvalidTopicName = errors.New("invalid topic name")
	ErrTopicNotFound    = errors.New("topic not found")
	ErrTopicArchived    = errors.New("topic is archived")
	ErrTopicConflict    = errors.New("topic name already in use")
	ErrStaleDelete      = errors.New("this deletion confirmation expired. Open it again from Archived")
	ErrDeletePending    = errors.New("topic removed from Archived, but some files remain. Cleanup will retry when the server restarts")
)

const archiveMarker = ".archived"

type TopicInfo struct {
	Name        string
	Archived    bool
	DeleteToken string
}

type Manager struct {
	mu           sync.RWMutex
	baseDir      string
	graphs       map[string]*TopicGraph
	deleteTokens map[string]string
	closed       bool
}

func NewManager(baseDir string) *Manager {
	os.MkdirAll(baseDir, 0755)
	cleanPendingDeletions(baseDir)
	return &Manager{
		baseDir:      baseDir,
		graphs:       make(map[string]*TopicGraph),
		deleteTokens: make(map[string]string),
	}
}

// cleanPendingDeletions runs before this manager starts serving requests.
func cleanPendingDeletions(baseDir string) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		log.Printf("oh-my-graph: scan for pending topic deletions: %v", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && deletionDirRE.MatchString(entry.Name()) {
			if err := os.RemoveAll(filepath.Join(baseDir, entry.Name())); err != nil {
				log.Printf("oh-my-graph: retry pending topic deletion: %v", err)
			}
		}
	}
}

func (m *Manager) Topics() ([]TopicInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return nil, err
	}
	topics := make([]TopicInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !topicNameRE.MatchString(entry.Name()) {
			continue
		}
		archived, err := isArchived(filepath.Join(m.baseDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		topic := TopicInfo{Name: entry.Name(), Archived: archived}
		if archived {
			token := m.deleteTokens[topic.Name]
			if token == "" {
				token = rand.Text()
				m.deleteTokens[topic.Name] = token
			}
			topic.DeleteToken = token
		}
		topics = append(topics, topic)
	}
	return topics, nil
}

func validateTopicName(name string) error {
	if !topicNameRE.MatchString(name) {
		return fmt.Errorf("%w %q: must match [a-zA-Z0-9_-]{1,64}", ErrInvalidTopicName, name)
	}
	return nil
}

// topicDir requires the actual directory entry's spelling. Stat alone would
// allow distinct cache keys for the same graph on case-insensitive filesystems.
func (m *Manager) topicDir(name string) (string, error) {
	dir := filepath.Join(m.baseDir, name)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return "", fmt.Errorf("%w: %q", ErrTopicNotFound, name)
	}
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() == name && entry.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrTopicNotFound, name)
}

func isArchived(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, archiveMarker))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// OpenTopic holds a manager lock until release. Callers must release it after
// their last graph operation, including any scheduling of asynchronous writes.
func (m *Manager) OpenTopic(name string, create bool) (*TopicGraph, func(), error) {
	if err := validateTopicName(name); err != nil {
		return nil, nil, err
	}
	m.mu.RLock()
	if g := m.graphs[name]; g != nil {
		return g, m.mu.RUnlock, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	g, err := m.openTopicLocked(name, create)
	if err != nil {
		m.mu.Unlock()
		return nil, nil, err
	}
	return g, m.mu.Unlock, nil
}

func (m *Manager) openTopicLocked(name string, create bool) (*TopicGraph, error) {
	if m.closed {
		return nil, errors.New("topic manager is closed")
	}
	if g := m.graphs[name]; g != nil {
		return g, nil
	}
	dir, err := m.topicDir(name)
	if create && errors.Is(err, ErrTopicNotFound) {
		dir = filepath.Join(m.baseDir, name)
		err = os.Mkdir(dir, 0755)
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %q", ErrTopicConflict, name)
		}
	}
	if err != nil {
		return nil, err
	}
	archived, err := isArchived(dir)
	if err != nil {
		return nil, err
	}
	if archived {
		return nil, fmt.Errorf("%w: %q", ErrTopicArchived, name)
	}
	g, err := newTopicGraph(filepath.Join(dir, "graph.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("load topic %q: %w", name, err)
	}
	m.graphs[name] = g
	return g, nil
}

func (m *Manager) RenameTopic(name, newName string) error {
	if err := validateTopicName(newName); err != nil {
		return err
	}
	return m.changeTopic(name, false, func(dir string) error {
		if name == newName {
			return nil
		}
		target := filepath.Join(m.baseDir, newName)
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("%w: %q", ErrTopicConflict, newName)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		m.evictTopic(name)
		return os.Rename(dir, target)
	})
}

func (m *Manager) ArchiveTopic(name string) error {
	return m.changeTopic(name, false, func(dir string) error {
		m.evictTopic(name)
		marker, err := os.OpenFile(filepath.Join(dir, archiveMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		return marker.Close()
	})
}

func (m *Manager) RestoreTopic(name string) error {
	return m.changeTopic(name, true, func(dir string) error {
		if err := os.Remove(filepath.Join(dir, archiveMarker)); err != nil {
			return err
		}
		delete(m.deleteTokens, name)
		return nil
	})
}

// DeleteTopic checks the archive confirmation while holding the manager lock.
// Move the whole directory out of the topic namespace before unlinking any files.
func (m *Manager) DeleteTopic(name, token string) error {
	var pendingDir string
	err := m.changeTopic(name, true, func(dir string) error {
		if token == "" || token != m.deleteTokens[name] {
			return ErrStaleDelete
		}
		var err error
		pendingDir, err = os.MkdirTemp(m.baseDir, deletionPrefix)
		if err != nil {
			return err
		}
		if err := os.Rename(dir, filepath.Join(pendingDir, "topic")); err != nil {
			os.Remove(pendingDir)
			return err
		}
		delete(m.deleteTokens, name)
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.RemoveAll(pendingDir); err != nil {
		return fmt.Errorf("%w: %w", ErrDeletePending, err)
	}
	return nil
}

func (m *Manager) changeTopic(name string, requireArchived bool, change func(string) error) error {
	if err := validateTopicName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("topic manager is closed")
	}
	dir, err := m.topicDir(name)
	if err != nil {
		return err
	}
	archived, err := isArchived(dir)
	if err != nil {
		return err
	}
	if archived && !requireArchived {
		return fmt.Errorf("%w: %q", ErrTopicArchived, name)
	}
	if !archived && requireArchived {
		return fmt.Errorf("%w: topic %q is already active", ErrTopicConflict, name)
	}
	return change(dir)
}

func (m *Manager) evictTopic(name string) {
	if g := m.graphs[name]; g != nil {
		g.Close()
		delete(m.graphs, name)
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for name := range m.graphs {
		m.evictTopic(name)
	}
}
