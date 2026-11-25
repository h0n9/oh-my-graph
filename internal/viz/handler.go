package viz

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/h0n9/oh-my-graph/internal/graph"
)

//go:embed static
var staticFS embed.FS

// The page file is listed first: ParseFS names the returned template after the
// first file, and shared.html only holds {{define}} blocks (an empty body if
// executed directly).
var indexTmpl = template.Must(template.ParseFS(staticFS, "static/index.html", "static/shared.html"))

type topicInfo struct {
	Name        string
	DeleteToken string
	NodeCount   int
	EdgeCount   int
}

type indexView struct {
	Archived      bool
	Error         string
	Topics        []topicInfo
	AgentsEnabled bool
	AgentCount    int
}

type Handler struct {
	manager *graph.Manager
	csrf    *http.CrossOriginProtection
	agents  AgentManager // nil unless enabled with WithAgents
	now     func() time.Time
}

func NewHandler(mgr *graph.Manager, opts ...Option) *Handler {
	h := &Handler{manager: mgr, csrf: http.NewCrossOriginProtection(), now: time.Now}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if servePWAAsset(w, r) {
		return
	}
	switch r.URL.Path {
	case "/", "":
		http.Redirect(w, r, "/topics", http.StatusFound)
	case "/topics":
		h.serveIndex(w, r.URL.Query().Get("tab") == "archived", http.StatusOK, "")
	case "/graph":
		http.ServeFileFS(w, r, staticFS, "static/graph.html")
	case "/api/graph":
		h.serveGraphData(w, r)
	case "/topics/rename", "/topics/archive", "/topics/restore", "/topics/delete":
		h.changeTopic(w, r)
	case "/agents", "/agents/revoke", "/agents/prune":
		if h.agents == nil {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/agents" {
			h.agentsPage(w, r)
		} else {
			h.changeAgents(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// servePWAAsset serves the PWA manifest and icon files, or reports false if
// the request path isn't one of them.
func servePWAAsset(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/manifest.json":
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFileFS(w, r, staticFS, "static/manifest.json")
	case "/icon-192.png":
		http.ServeFileFS(w, r, staticFS, "static/icon-192.png")
	case "/icon-512.png":
		http.ServeFileFS(w, r, staticFS, "static/icon-512.png")
	case "/apple-touch-icon.png":
		http.ServeFileFS(w, r, staticFS, "static/apple-touch-icon.png")
	default:
		return false
	}
	return true
}

// PWAAssets returns a handler for the PWA manifest and icon files alone.
// It's meant to be mounted outside of any owner-session wrapping applied to
// the rest of the viz UI: Safari's "Add to Home Screen" flow fetches these
// in the background without attaching interactively-entered credentials,
// so gating them the same way the graph pages are gated silently breaks
// the install icon. They carry no graph data -- just app name, theme
// colors, and a generic icon -- so exposing them is safe.
func PWAAssets() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servePWAAsset(w, r) {
			http.NotFound(w, r)
		}
	})
}

func (h *Handler) serveIndex(w http.ResponseWriter, archived bool, status int, message string) {
	view := indexView{Archived: archived, Error: message}
	if h.agents != nil {
		view.AgentsEnabled = true
		view.AgentCount = h.agentCount()
	}
	topics, err := h.manager.Topics()
	if err != nil {
		log.Printf("oh-my-graph: list topics: %v", err)
		status = http.StatusInternalServerError
		view.Error = "Could not load topics."
	}
	for _, topic := range topics {
		if topic.Archived != archived {
			continue
		}
		info := topicInfo{Name: topic.Name, DeleteToken: topic.DeleteToken}
		if !archived {
			g, release, err := h.manager.OpenTopic(topic.Name, false)
			if err != nil {
				continue
			}
			_, info.NodeCount, info.EdgeCount = g.Stats()
			release()
		}
		view.Topics = append(view.Topics, info)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := indexTmpl.Execute(w, view); err != nil {
		log.Printf("oh-my-graph: render topics: %v", err)
	}
}

func topicErrorStatus(err error) int {
	switch {
	case errors.Is(err, graph.ErrInvalidTopicName):
		return http.StatusBadRequest
	case errors.Is(err, graph.ErrTopicNotFound):
		return http.StatusNotFound
	case errors.Is(err, graph.ErrTopicArchived), errors.Is(err, graph.ErrTopicConflict), errors.Is(err, graph.ErrStaleDelete):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// maxFormBytes caps the request body of every form POST.
const maxFormBytes = 4 << 10

// acceptFormPost reports whether r is a POST that passes the cross-origin
// check and whose body (at most maxFormBytes) parses as a form. When it
// returns false a response has already been written: a 405 (with Allow: POST)
// or 403 by acceptFormPost itself, or by onBadForm, which must write the
// response when the form can't be read.
// noun names what the form changes in the 403 message ("topic", "agent").
func (h *Handler) acceptFormPost(w http.ResponseWriter, r *http.Request, noun string, onBadForm func()) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := h.csrf.Check(r); err != nil {
		http.Error(w, "cross-origin "+noun+" changes are forbidden", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		onBadForm()
		return false
	}
	return true
}

func (h *Handler) changeTopic(w http.ResponseWriter, r *http.Request) {
	archived := r.URL.Path == "/topics/restore" || r.URL.Path == "/topics/delete"
	if !h.acceptFormPost(w, r, "topic", func() {
		h.serveIndex(w, archived, http.StatusBadRequest, "Invalid topic form.")
	}) {
		return
	}

	topic := r.PostForm.Get("topic")
	var err error
	switch r.URL.Path {
	case "/topics/rename":
		err = h.manager.RenameTopic(topic, r.PostForm.Get("new_name"))
	case "/topics/archive":
		err = h.manager.ArchiveTopic(topic)
	case "/topics/restore":
		err = h.manager.RestoreTopic(topic)
	case "/topics/delete":
		err = h.manager.DeleteTopic(topic, r.PostForm.Get("delete_token"))
	}
	if err != nil {
		status, message := topicErrorStatus(err), err.Error()
		if status == http.StatusInternalServerError {
			log.Printf("oh-my-graph: %s: %v", r.URL.Path, err)
			message = "Could not update topic."
			if errors.Is(err, graph.ErrDeletePending) {
				message = graph.ErrDeletePending.Error()
			} else if r.URL.Path == "/topics/delete" {
				message = "Could not delete topic."
			}
		}
		h.serveIndex(w, archived, status, message)
		return
	}
	target := "/topics"
	if archived {
		target = "/topics?tab=archived"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

type graphData struct {
	Instance string     `json:"instance"`
	Nodes    []nodeView `json:"nodes"`
	Edges    []edgeView `json:"edges"`
}

type nodeView struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Confidence  *float64 `json:"confidence,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type edgeView struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (h *Handler) serveGraphData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}

	g, release, err := h.manager.OpenTopic(topic, false)
	if err != nil {
		status, message := topicErrorStatus(err), err.Error()
		if status == http.StatusInternalServerError {
			log.Printf("oh-my-graph: load graph: %v", err)
			message = "Could not load graph."
		}
		http.Error(w, message, status)
		return
	}

	nodes, edges := g.Snapshot()
	instance := g.Instance()
	release()

	data := graphData{
		Instance: instance,
		Nodes:    make([]nodeView, 0, len(nodes)),
		Edges:    make([]edgeView, 0, len(edges)),
	}
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, nodeView{
			ID:          n.NodeID,
			Type:        string(n.Type),
			Summary:     n.Summary,
			Description: n.Description,
			Confidence:  n.Confidence,
			Tags:        append([]string(nil), n.Tags...),
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
