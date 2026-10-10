package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/web"
)

// serve runs web mode: the multi-session manager (registered projects,
// lazily opened sessions) behind the UI, plus a debug REPL that accepts
// commands only — free-form input goes to the web page, so the REPL can
// never race a page for a session's single run slot (and the hubs stay
// the only consumers of the run event streams).
func serve(serverRoot, addr, providerName, modelName, resumeID, staticDir string) {
	factory, err := assembleFactory(serverRoot, providerName, modelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		os.Exit(1)
	}
	manager, err := web.NewManager(web.ManagerOptions{
		ServerRoot: serverRoot,
		Store:      factory.Store(),
		AgentFor: func(projectCwd string) (*agent.Agent, error) {
			ag, _, err := factory.Agent(projectCwd)
			return ag, err
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		os.Exit(1)
	}
	// -session pre-opens through the same registration gate as every
	// route: the lock is claimed up front and the page initially
	// selects it.
	if resumeID != "" {
		if err := manager.Preopen(resumeID); err != nil {
			fmt.Fprintf(os.Stderr, "sai: %v\n", err)
			manager.Close()
			os.Exit(1)
		}
	}
	// The chat client is an external build; mounting a directory that
	// does not exist would serve a broken /chat, so fail fast.
	if staticDir != "" {
		if info, err := os.Stat(staticDir); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "sai: -http-static: not a directory: %s\n", staticDir)
			os.Exit(1)
		}
	}
	server := web.New(manager, web.WithStaticDir(staticDir))
	httpServer := &http.Server{
		Handler: server.Handler(),
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		manager.Close()
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "sai: web UI on http://%s (server root %s)\n", listener.Addr(), serverRoot)
	if staticDir != "" {
		fmt.Fprintf(os.Stderr, "sai: chat client at /chat (from %s), log view at /log\n", staticDir)
	} else {
		fmt.Fprintf(os.Stderr, "sai: log view at /log (chat client not mounted; -http-static <dir>)\n")
	}
	fmt.Fprintf(os.Stderr, "sai: serve mode: the interactive REPL is disabled; this console accepts debug commands only\n")

	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "sai: http server: %v\n", err)
		}
	}()

	// Orderly shutdown shared by /quit and the interrupt path: stop
	// accepting, stop the manager (sweeper, every hub and session —
	// flushing logs, releasing every session lock), release the root
	// lock.
	shutdown := func() {
		_ = httpServer.Close()
		server.Close()
	}

	// Ctrl+C mirrors the REPL: the first stops every active run, a
	// second when idle exits through the orderly path.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		for range interrupts {
			if anyRunning(manager) {
				manager.StopRuns()
				fmt.Fprintln(os.Stderr, "\rsai: stopping runs")
				continue
			}
			fmt.Fprintln(os.Stderr, "\rsai: shutting down")
			shutdown()
			os.Exit(130)
		}
	}()

	fmt.Fprintln(os.Stderr, "sai: /help /quit")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for {
		fmt.Fprint(os.Stderr, "sai> ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if !strings.HasPrefix(text, "/") {
			fmt.Fprintln(os.Stderr, "sai: serve mode takes commands only; type your message on the web page")
			continue
		}
		if text == "/quit" || text == "/exit" {
			break
		}
		if text == "/help" {
			fmt.Fprintln(os.Stderr, "sai: /quit  stop the server and exit")
			fmt.Fprintln(os.Stderr, "sai:       (more debug commands to come)")
			continue
		}
		fmt.Fprintf(os.Stderr, "sai: unknown command %q (try /help)\n", text)
	}
	shutdown()
}

// anyRunning reports whether any open session has an active run.
func anyRunning(manager *web.Manager) bool {
	return manager.HasRunning()
}
