package graph

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTopicLifecycle(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	t.Cleanup(mgr.Close)
	g, release, err := mgr.OpenTopic("first", true)
	if err != nil {
		t.Fatal(err)
	}
	oldInstance := g.Instance()
	renamed := make(chan error, 1)
	go func() { renamed <- mgr.RenameTopic("first", "second") }()
	cursor, writeErr := g.Write(
		[]*Node{{NodeID: "a", Type: NodeTypeFinding}, {NodeID: "b", Type: NodeTypeFinding}},
		[]*Edge{{EdgeID: "e", Type: EdgeTypeReferences, FromNodeID: "a", ToNodeID: "b"}},
	)
	release()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if err := <-renamed; err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(filepath.Join(dir, "second", "graph.jsonl"))
	if err != nil || len(original) == 0 {
		t.Fatalf("read renamed WAL: %d bytes, %v", len(original), err)
	}
	assertTopicOpenError(t, mgr, "first", false, ErrTopicNotFound)
	if err := mgr.ArchiveTopic("second"); err != nil {
		t.Fatal(err)
	}
	mgr.Close()

	mgr = NewManager(dir)
	t.Cleanup(mgr.Close)
	topics, err := mgr.Topics()
	if err != nil || len(topics) != 1 || topics[0] != (TopicInfo{Name: "second", Archived: true}) {
		t.Fatalf("archived topics after restart: %v, %v", topics, err)
	}
	assertTopicOpenError(t, mgr, "second", true, ErrTopicArchived)
	if err := mgr.RenameTopic("second", "second"); !errors.Is(err, ErrTopicArchived) {
		t.Fatalf("rename archived topic to same name: %v", err)
	}
	if err := mgr.RestoreTopic("second"); err != nil {
		t.Fatal(err)
	}

	restored, release, err := mgr.OpenTopic("second", false)
	if err != nil {
		t.Fatal(err)
	}
	seq, nodes, edges := restored.Stats()
	instance := restored.Instance()
	release()
	if seq != cursor || nodes != 2 || edges != 1 || instance == oldInstance {
		t.Fatalf("restored graph: cursor %d, %d nodes, %d edges, instance %q", seq, nodes, edges, instance)
	}
	wal, err := os.ReadFile(filepath.Join(dir, "second", "graph.jsonl"))
	if err != nil || string(wal) != string(original) {
		t.Fatalf("WAL changed during archive and restore: %v", err)
	}
}

func TestRenameRejectsOccupiedPaths(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()
	for _, name := range []string{"source", "reserved"} {
		_, release, err := mgr.OpenTopic(name, true)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if err := mgr.ArchiveTopic("reserved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"reserved", "file", "link"} {
		if err := mgr.RenameTopic("source", target); !errors.Is(err, ErrTopicConflict) {
			t.Errorf("rename to %s: %v", target, err)
		}
	}
	if err := mgr.RenameTopic("source", "source"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RenameTopic("missing", "missing"); !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("rename missing to same name: %v", err)
	}
}

func TestTopicCaseAliases(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()
	g, release, err := mgr.OpenTopic("Foo", true)
	if err != nil {
		t.Fatal(err)
	}
	original := g.Instance()
	release()
	_, err = os.Lstat(filepath.Join(dir, "foo"))
	if errors.Is(err, os.ErrNotExist) {
		alias, release, err := mgr.OpenTopic("foo", true)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if alias.Instance() == original {
			t.Fatal("distinct directories share one graph")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	assertTopicOpenError(t, mgr, "foo", false, ErrTopicNotFound)
	assertTopicOpenError(t, mgr, "foo", true, ErrTopicConflict)
	if err := mgr.RenameTopic("Foo", "foo"); !errors.Is(err, ErrTopicConflict) {
		t.Errorf("rename to alias: %v", err)
	}
	if err := mgr.ArchiveTopic("foo"); !errors.Is(err, ErrTopicNotFound) {
		t.Errorf("archive by alias: %v", err)
	}
}

func assertTopicOpenError(t *testing.T, mgr *Manager, name string, create bool, want error) {
	t.Helper()
	_, release, err := mgr.OpenTopic(name, create)
	if release != nil {
		release()
	}
	if !errors.Is(err, want) {
		t.Fatalf("open %q (create=%t): %v, want %v", name, create, err, want)
	}
}

func TestArchiveFailureReopensActiveGraph(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires filesystem permissions to apply to the test process")
	}
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()
	g, release, err := mgr.OpenTopic("topic", true)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := g.Write([]*Node{{NodeID: "a", Type: NodeTypeFinding}}, nil)
	release()
	if err != nil {
		t.Fatal(err)
	}

	topicDir := filepath.Join(dir, "topic")
	if err := os.Chmod(topicDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(topicDir, 0755) })
	if err := mgr.ArchiveTopic("topic"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("archive read-only directory: %v", err)
	}

	reopened, release, err := mgr.OpenTopic("topic", false)
	if err != nil {
		t.Fatal(err)
	}
	seq, nodes, _ := reopened.Stats()
	release()
	if reopened == g || seq != cursor || nodes != 1 {
		t.Fatalf("active graph did not reopen with its original data: cursor %d, nodes %d", seq, nodes)
	}
}
