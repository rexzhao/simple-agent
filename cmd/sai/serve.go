package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/rexzhao/simple-agent/internal/web"
)

// serve runs web mode: the UI server plus a debug REPL that accepts
// commands only — free-form input goes to the web page, so the REPL can
// never race the page for the session's single run slot (and the hub
// stays the only consumer of the run event stream).
func serve(cli *cliApp, addr string) {
	// The web page is the display surface now; the terminal becomes a
	// pure command console (/tail re-enables live terminal output).
	cli.stopWatcher()
	server := web.New(cli.session)
	httpServer := &http.Server{
		Handler: server.Handler(),
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sai: %v\n", err)
		_ = cli.session.Close()
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "sai: web UI on http://%s (session %s)\n", listener.Addr(), cli.session.ID())
	fmt.Fprintln(os.Stderr, "sai: serve mode: the interactive REPL is disabled; this console accepts debug commands only")

	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "sai: http server: %v\n", err)
		}
	}()

	// Orderly shutdown shared by /quit and the interrupt path: stop
	// accepting, stop any run, close the session (flushes the log,
	// finishes any segment switch, releases the session lock), and name
	// the session for a later resume.
	shutdown := func() {
		_ = httpServer.Close()
		server.Close()
		_ = cli.session.Close()
		if cli.session.Persisted() {
			fmt.Fprintf(os.Stderr, "sai: session %s\n", cli.session.ID())
		}
	}

	// Ctrl+C mirrors the REPL: the first stops the active run, a second
	// when idle exits through the orderly path.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		for range interrupts {
			if cli.session.Running() {
				cli.session.Stop()
				fmt.Fprintln(os.Stderr, "\rsai: stopping run")
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
