package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/rexzhao/simple-agent/internal/execution"
)

const usageText = `usage: sai [--listen 127.0.0.1:0] [--server-root dir] [--cwd dir] [--no-open]

Starts the local SAI Web application. With no arguments, SAI listens on a
random loopback port and opens the application in the default browser.

Options:
  --listen addr      Loopback listen address (default 127.0.0.1:0)
  --server-root dir  Project and session storage root
  --cwd dir          Initial working-directory hint shown in the Web UI
  --no-open          Do not open the browser automatically
  --version          Print version and exit
`

func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sai", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listenAddress := flags.String("listen", "127.0.0.1:0", "loopback listen address")
	serverRoot := flags.String("server-root", "", "storage root")
	cwd := flags.String("cwd", "", "initial working directory")
	noOpen := flags.Bool("no-open", false, "do not open browser")
	showVersion := flags.Bool("version", false, "print version")
	showHelp := flags.Bool("help", false, "print help")
	flags.BoolVar(showHelp, "h", false, "print help")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(stderr, "sai: %v\n\n%s", err, usageText)
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "sai: unexpected argument %q\n\n%s", flags.Arg(0), usageText)
		return 1
	}
	if *showHelp {
		fmt.Fprint(stdout, usageText)
		return 0
	}
	if *showVersion {
		fmt.Fprintln(stdout, Version)
		return 0
	}

	root, err := resolveStorageRoot(*serverRoot)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	initialCWD, err := resolveInitialCWD(*cwd)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	listener, err := listenLoopback(*listenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	defer listener.Close()

	service, err := execution.NewServiceWithAgentRunner(root)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	token, err := capabilityToken()
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := NewServer(ServerOptions{Context: ctx, Service: service, Token: token, CWD: initialCWD})
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	httpServer := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- httpServer.Serve(listener)
	}()

	url := "http://" + listener.Addr().String() + "/#token=" + token
	fmt.Fprintf(stdout, "SAI_WEB_URL\t%s\n", url)
	if !*noOpen {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(stderr, "sai: open browser: %v\n", err)
		}
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "sai: shutdown: %v\n", err)
			return 1
		}
		return 0
	case err := <-serveDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "sai: web server: %v\n", err)
			return 1
		}
		return 0
	}
}

func resolveStorageRoot(explicit string) (string, error) {
	root := strings.TrimSpace(explicit)
	if root == "" {
		root = strings.TrimSpace(os.Getenv("SAI_SERVER_ROOT"))
	}
	if root == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("find user config directory: %w", err)
		}
		root = filepath.Join(configDir, "sai")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve storage root %q: %w", root, err)
	}
	return filepath.Clean(abs), nil
}

func resolveInitialCWD(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		return filepath.Clean(cwd), nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve cwd %q: %w", value, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat cwd %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

func listenLoopback(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("listen address must use a loopback host")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	return listener, nil
}

func capabilityToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate capability token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func openBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	return command.Start()
}
