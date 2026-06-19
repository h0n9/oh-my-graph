package graph

import (
	"encoding/json"
	"time"
)

type NodeType string

const (
	NodeTypeFinding  NodeType = "finding"
	NodeTypeConcept  NodeType = "concept"
	NodeTypeBlocker  NodeType = "blocker"
	NodeTypeQuestion NodeType = "question"
	NodeTypeDecision NodeType = "decision"
	NodeTypeArtifact NodeType = "artifact"
	NodeTypeEntity   NodeType = "entity"
	NodeTypeEvent    NodeType = "event"
	NodeTypeMessage  NodeType = "message"
)

type EdgeType string

const (
	EdgeTypeResolves    EdgeType = "resolves"
	EdgeTypeProduces    EdgeType = "produces"
	EdgeTypeBlocks      EdgeType = "blocks"
	EdgeTypeCauses      EdgeType = "causes"
	EdgeTypeSupports    EdgeType = "supports"
	EdgeTypeContradicts EdgeType = "contradicts"
	EdgeTypeDependsOn   EdgeType = "depends_on"
	EdgeTypePartOf      EdgeType = "part_of"
	EdgeTypeReferences  EdgeType = "references"
	EdgeTypeRepliesTo   EdgeType = "replies_to"
	EdgeTypeDeprecates  EdgeType = "deprecates"
)

type Node struct {
	NodeID      string   `json:"node_id"`
	Type        NodeType `json:"type"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Confidence  float64  `json:"confidence"`
}

type Edge struct {
	EdgeID     string   `json:"edge_id"`
	Type       EdgeType `json:"type"`
	FromNodeID string   `json:"from_node_id"`
	ToNodeID   string   `json:"to_node_id"`
}

type WALRecord struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	TS   time.Time       `json:"ts"`
	Data json.RawMessage `json:"data"`
}

type NodeSummary struct {
	NodeID  string `json:"node_id"`
	Summary string `json:"summary"`
	Seq     int64  `json:"seq"`
}

type NodeWithEdges struct {
	Node  *Node   `json:"node"`
	Edges []*Edge `json:"edges"`
}
