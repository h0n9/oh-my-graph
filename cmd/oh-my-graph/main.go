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

	"github.com/h0n9/oh-my-graph/internal/graph"
	"github.com/h0n9/oh-my-graph/internal/mcp"
	"github.com/h0n9/oh-my-graph/internal/viz"
)

func main() {
	port := flag.Int("port", 7780, "HTTP listen port")
	data := flag.String("data", "", "data directory (default: ~/.oh-my-graph)")
	flag.Parse()

	dir := resolveDir(*data)
	mgr := graph.NewManager(dir)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewServer(mgr))
	mux.Handle("/", viz.NewHandler(mgr))

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
