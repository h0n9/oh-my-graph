package graph

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateTags(t *testing.T) {
	valid := []string{"a", "tag-search", strings.Repeat("a", MaxTagLength)}
	if err := ValidateTags(valid); err != nil {
		t.Fatalf("valid tags rejected: %v", err)
	}
	for _, tags := range [][]string{
		{"Upper"}, {"white space"}, {"under_score"}, {"status:open"}, {"*"},
		{"-leading"}, {"trailing-"}, {"double--hyphen"}, {strings.Repeat("a", MaxTagLength+1)},
		{"same", "same"},
	} {
		if err := ValidateTags(tags); err == nil {
			t.Fatalf("invalid tags accepted: %q", tags)
		}
	}
	tooMany := make([]string, MaxTagsPerNode+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("tag-%d", i)
	}
	if err := ValidateTags(tooMany); err == nil {
		t.Fatal("too many tags accepted")
	}
	atLimit := tooMany[:MaxTagsPerNode]
	if err := ValidateTags(atLimit); err != nil {
		t.Fatalf("maximum tag count rejected: %v", err)
	}
}

func TestTagSearchAndList(t *testing.T) {
	g, _ := newTestGraph(t)
	_, err := g.Write([]*Node{
		{NodeID: "n1", Type: NodeTypeFinding, Summary: "one", Tags: []string{"graph", "search"}},
		{NodeID: "n2", Type: NodeTypeDecision, Summary: "two", Tags: []string{"graph", "design"}},
		{NodeID: "n3", Type: NodeTypeFinding, Summary: "three", Tags: []string{"graph", "search", "design"}},
		{NodeID: "n4", Type: NodeTypeFinding, Summary: "four"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	all := g.SearchByTags([]string{"graph", "search"}, "all", nil, 50)
	if got := []string{all[0].NodeID, all[1].NodeID}; !reflect.DeepEqual(got, []string{"n3", "n1"}) {
		t.Fatalf("AND order = %v", got)
	}
	any := g.SearchByTags([]string{"search", "design"}, "any", nil, 50)
	if got := []string{any[0].NodeID, any[1].NodeID, any[2].NodeID}; !reflect.DeepEqual(got, []string{"n3", "n2", "n1"}) {
		t.Fatalf("OR order/dedup = %v", got)
	}
	filtered := g.SearchByTags([]string{"graph"}, "all", []NodeType{NodeTypeDecision}, 50)
	if len(filtered) != 1 || filtered[0].NodeID != "n2" {
		t.Fatalf("type filter = %+v", filtered)
	}
	if got := g.SearchByTags([]string{"missing"}, "all", nil, 50); got == nil || len(got) != 0 {
		t.Fatalf("empty search = %#v", got)
	}

	counts := g.ListTags(2)
	want := []TagCount{{Tag: "graph", NodeCount: 3}, {Tag: "design", NodeCount: 2}}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("ListTags = %+v, want %+v", counts, want)
	}
	if got := g.ListTags(10); len(got) != 3 || got[1].Tag != "design" || got[2].Tag != "search" {
		t.Fatalf("lexical tie order = %+v", got)
	}
}

func TestTagsPersistAndDoNotAlias(t *testing.T) {
	g, path := newTestGraph(t)
	tags := []string{"graph", "search"}
	n := &Node{NodeID: "n1", Type: NodeTypeFinding, Summary: "one", Tags: tags}
	batch := []*Node{n}
	if _, err := g.Write(batch, nil); err != nil {
		t.Fatal(err)
	}
	tags[0] = "mutated"
	n.Tags[1] = "changed"
	batch[0].Tags[0] = "batch-mutation"
	got, _ := g.GetNode("n1")
	if !reflect.DeepEqual(got.Node.Tags, []string{"graph", "search"}) {
		t.Fatalf("stored tags aliased input: %v", got.Node.Tags)
	}
	got.Node.Tags[0] = "output-mutation"
	again, _ := g.GetNode("n1")
	if again.Node.Tags[0] != "graph" {
		t.Fatalf("read tags alias graph state: %v", again.Node.Tags)
	}
	g.Close()

	reloaded, err := newTopicGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if matches := reloaded.SearchByTags([]string{"search"}, "all", nil, 10); len(matches) != 1 {
		t.Fatalf("reloaded index = %+v", matches)
	}
}

func TestTagWriteValidationIsAtomic(t *testing.T) {
	g, _ := newTestGraph(t)
	if _, err := g.Write([]*Node{
		{NodeID: "valid", Type: NodeTypeFinding, Summary: "valid", Tags: []string{"good"}},
		{NodeID: "invalid", Type: NodeTypeFinding, Summary: "invalid", Tags: []string{"bad:tag"}},
	}, nil); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if _, count, _ := g.Stats(); count != 0 {
		t.Fatalf("partial batch written: %d nodes", count)
	}
}

func TestTagSearchConcurrentWrite(t *testing.T) {
	g, _ := newTestGraph(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = g.Write([]*Node{{NodeID: fmt.Sprintf("n-%d", i), Type: NodeTypeFinding, Summary: "n", Tags: []string{"common"}}}, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = g.SearchByTags([]string{"common"}, "all", nil, 10)
			_ = g.ListTags(10)
		}
	}()
	wg.Wait()
}

func BenchmarkSearchByTagsSelective(b *testing.B) {
	g, _ := newTestGraph(b)
	nodes := make([]*Node, 10000)
	for i := range nodes {
		tags := []string{"common"}
		if i%1000 == 0 {
			tags = append(tags, "rare")
		}
		nodes[i] = &Node{NodeID: fmt.Sprintf("n-%d", i), Type: NodeTypeFinding, Summary: "n", Tags: tags}
	}
	_, _ = g.Write(nodes, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.SearchByTags([]string{"common", "rare"}, "all", nil, 10)
	}
}

func BenchmarkSearchByTagsCommon(b *testing.B) {
	g, _ := newTestGraph(b)
	nodes := make([]*Node, 10000)
	for i := range nodes {
		nodes[i] = &Node{NodeID: fmt.Sprintf("n-%d", i), Type: NodeTypeFinding, Summary: "n", Tags: []string{"common"}}
	}
	_, _ = g.Write(nodes, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.SearchByTags([]string{"common"}, "all", nil, 50)
	}
}

func BenchmarkSearchByTagsUnion(b *testing.B) {
	g, _ := newTestGraph(b)
	nodes := make([]*Node, 10000)
	for i := range nodes {
		nodes[i] = &Node{NodeID: fmt.Sprintf("n-%d", i), Type: NodeTypeFinding, Summary: "n", Tags: []string{fmt.Sprintf("group-%d", i%4)}}
	}
	_, _ = g.Write(nodes, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.SearchByTags([]string{"group-0", "group-1", "group-2"}, "any", nil, 50)
	}
}

func BenchmarkTagIndexBuild(b *testing.B) {
	nodes := make([]*Node, 10000)
	for i := range nodes {
		nodes[i] = &Node{NodeID: fmt.Sprintf("n-%d", i), Type: NodeTypeFinding, Tags: []string{"common", fmt.Sprintf("group-%d", i%100)}}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g := &TopicGraph{
			nodes: make(map[string]*Node, len(nodes)), typeLog: make(map[NodeType][]nodeLogEntry),
			tagLog: make(map[string][]nodeLogEntry), nodeSeq: make(map[string]int64, len(nodes)),
		}
		for seq, n := range nodes {
			g.nodes[n.NodeID] = n
			g.indexNode(int64(seq+1), time.Time{}, n)
		}
	}
}
