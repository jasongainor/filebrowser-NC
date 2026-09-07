// Command cncd is the standalone CNC daemon: the headless half of the
// filebrowser/CNC split described in docs/REPO_SPLIT_TODO.md and
// docs/CNCD.md. It serves the CNC API — /api/cnc/*, /api/displays/{id},
// and a minimal /api/files/* — from a single directory, with no
// filebrowser users, bolt DB, or Vue frontend behind it.
//
// Not here yet, on purpose (see docs/CNCD.md): a login screen (a
// later step validates web logins against Samba), serial passthrough
// beyond the existing Haas bridge protocol, and a UI. The only
// identity cncd understands today is "presented the configured
// machine token as a bearer" vs. everyone else.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/filebrowser/filebrowser/v2/cnc"
	"github.com/filebrowser/filebrowser/v2/cncd"
)

func main() {
	root := flag.String("root", "", "directory cncd serves (the Samba share); required")
	listen := flag.String("listen", ":8080", "address to listen on")
	configPath := flag.String("config", "", "path to the JSON config file (machines, displays, discord, machineToken, baselinePollSeconds); required")
	flag.Parse()

	if *root == "" {
		log.Fatal("cncd: --root is required")
	}
	if *configPath == "" {
		log.Fatal("cncd: --config is required")
	}
	if info, err := os.Stat(*root); err != nil || !info.IsDir() {
		log.Fatalf("cncd: --root %q is not a directory: %v", *root, err)
	}

	store, err := cncd.LoadStore(*configPath)
	if err != nil {
		log.Fatalf("cncd: %v", err)
	}

	registry := cnc.NewRegistry(store)
	defer registry.Stop()

	handler := cncd.NewRouter(cncd.Deps{
		Registry: registry,
		Config:   store,
		Root:     *root,
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("cncd: serving %s on %s", *root, *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("cncd: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Println("cncd: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("cncd: shutdown: %v", err)
	}
}
