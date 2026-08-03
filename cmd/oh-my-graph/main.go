package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/h0n9/oh-my-graph/internal/auth"
	"github.com/h0n9/oh-my-graph/internal/graph"
	"github.com/h0n9/oh-my-graph/internal/mcp"
	"github.com/h0n9/oh-my-graph/internal/viz"
)

// Version is set at build time via -ldflags "-X main.Version=<tag>".
var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(Version)
		os.Exit(0)
	}

	port := flag.Int("port", 7780, "HTTP listen port")
	data := flag.String("data", "", "data directory (default: ~/.oh-my-graph)")
	authEnabled := flag.Bool("auth", false, "require owner auth (OAuth 2.1 DCR+PKCE bearer on /mcp and /omg-mcp, HTTP Basic on the viz UI) using a shared passphrase; reads OMG_ISSUER and OMG_OWNER_PASSPHRASE")
	flag.Parse()

	dir := resolveDir(*data)
	mgr := graph.NewManager(dir)

	mux := http.NewServeMux()
	mcpServer := mcp.NewServer(mgr)

	var mcpHandler http.Handler = mcpServer
	var vizHandler http.Handler = viz.NewHandler(mgr)
	if *authEnabled {
		issuer := os.Getenv("OMG_ISSUER")
		passphrase := os.Getenv("OMG_OWNER_PASSPHRASE")
		if issuer == "" || passphrase == "" {
			log.Fatal("oh-my-graph: --auth requires OMG_ISSUER and OMG_OWNER_PASSPHRASE to be set")
		}
		authSrv := auth.NewServer(auth.Config{
			Issuer:          issuer,
			OwnerPassphrase: passphrase,
			ClientsFile:     filepath.Join(dir, "oauth-clients.json"),
		})
		authSrv.RegisterRoutes(mux)
		mcpHandler = authSrv.RequireBearer(mcpServer)
		// The viz UI is browser-facing, not an MCP client, so it can't carry a
		// Bearer header on plain navigation — gate it with a session cookie
		// from /login instead, using the same owner passphrase.
		vizHandler = authSrv.RequireOwnerSession(vizHandler)
	}

	mux.Handle("/mcp", mcpHandler)
	// Some hosts (e.g. Sprites' public gateway) reserve "/mcp" for their own
	// control-plane MCP server; expose the same handler under an alias so
	// oh-my-graph's MCP endpoint is still reachable there.
	mux.Handle("/omg-mcp", mcpHandler)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":%q}`, Version)
	})
	// The PWA manifest/icons must stay reachable without auth even when
	// --auth gates the rest of the viz UI (see viz.PWAAssets doc comment) --
	// registered directly on mux so they win over "/" regardless of the
	// Basic Auth wrapping below.
	pwaAssets := viz.PWAAssets()
	mux.Handle("/manifest.json", pwaAssets)
	mux.Handle("/icon-192.png", pwaAssets)
	mux.Handle("/icon-512.png", pwaAssets)
	mux.Handle("/apple-touch-icon.png", pwaAssets)
	mux.Handle("/", vizHandler)

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig

		log.Println("oh-my-graph: shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
		mgr.Close()
	}()

	log.Printf("oh-my-graph: listening on :%d, data at %s", *port, dir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("oh-my-graph: %v", err)
	}
}

func resolveDir(data string) string {
	if data != "" {
		return data
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".oh-my-graph"
	}
	return filepath.Join(home, ".oh-my-graph")
}
