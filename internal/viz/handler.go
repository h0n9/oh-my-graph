package viz

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

//go:embed static
var staticFS embed.FS

var indexTmpl = template.Must(template.ParseFS(staticFS, "static/index.html"))

type topicInfo struct {
	Name      string
	NodeCount int
	EdgeCount int
}

type Handler struct {
	manager *graph.Manager
}

func NewHandler(mgr *graph.Manager) *Handler {
	return &Handler{manager: mgr}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "":
		h.serveIndex(w, r)
	case "/graph":
		http.ServeFileFS(w, r, staticFS, "static/graph.html")
	case "/api/graph":
		h.serveGraphData(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	names := h.manager.Topics()
	infos := make([]topicInfo, 0, len(names))
	for _, name := range names {
		g, err := h.manager.Topic(name)
		if err != nil {
			continue
		}
		_, nodeCount, edgeCount := g.Stats()
		infos = append(infos, topicInfo{Name: name, NodeCount: nodeCount, EdgeCount: edgeCount})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexTmpl.Execute(w, infos)
}

type graphData struct {
	Nodes []nodeView `json:"nodes"`
	Edges []edgeView `json:"edges"`
}

type nodeView struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Summary    string  `json:"summary"`
	Confidence float64 `json:"confidence"`
}

type edgeView struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (h *Handler) serveGraphData(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}

	g, err := h.manager.Topic(topic)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	nodes, edges := g.Snapshot()

	data := graphData{
		Nodes: make([]nodeView, 0, len(nodes)),
		Edges: make([]edgeView, 0, len(edges)),
	}
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, nodeView{
			ID:         n.NodeID,
			Type:       string(n.Type),
			Summary:    n.Summary,
			Confidence: n.Confidence,
		})
	}
	for _, e := range edges {
		data.Edges = append(data.Edges, edgeView{
			ID:   e.EdgeID,
			Type: string(e.Type),
			From: e.FromNodeID,
			To:   e.ToNodeID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
