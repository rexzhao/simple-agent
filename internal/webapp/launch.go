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

const usageText = `usage: sai [--listen 127.0.0.1:0] [--allow-non-loopback] [--server-root dir] [--cwd dir] [--no-open]

Starts the local SAI Web application. With no arguments, SAI listens on a
random loopback port and opens the application in the default browser.

Options:
  --listen addr          Loopback listen address (default 127.0.0.1:0)
  --allow-non-loopback   Permit a non-loopback --listen address (INSECURE:
                         the web UI is reachable by anyone who can reach
                         the interface; use only on trusted networks)
  --server-root dir      Configuration and durable data namespace root
  --cwd dir              Initial working-directory hint shown in the Web UI
  --no-open              Do not open the browser automatically
  --version              Print version and exit
`

func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sai", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listenAddress := flags.String("listen", "127.0.0.1:0", "loopback listen address")
	allowNonLoopback := flags.Bool("allow-non-loopback", false, "permit a non-loopback listen address")
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

	basename, err := commandBasename(os.Args[0])
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	root, err := resolveStorageRoot(*serverRoot, basename)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	initialCWD, err := resolveInitialCWD(*cwd)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	instanceLock, exitCode, mustExit, err := acquireInstance(root, instanceAcquireOptions{
		noOpen: *noOpen,
		stdout: stdout,
		stderr: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	if mustExit {
		return exitCode
	}
	defer instanceLock.Release()
	listener, err := listen(*listenAddress, *allowNonLoopback)
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	defer listener.Close()
	if !isLoopbackAddress(*listenAddress) {
		fmt.Fprintf(stderr, "sai: WARNING: listening on a non-loopback address; the web UI is reachable by other hosts on the network\n")
	}

	configPath := filepath.Join(root, basename+".yaml")
	service, err := execution.NewServiceWithAgentRunner(root, configPath)
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
	app, err := NewServer(ServerOptions{Context: ctx, Service: service, Token: token, CWD: initialCWD, LogWriter: stderr, AllowNonLoopback: !isLoopbackAddress(*listenAddress)})
	if err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	defer app.Close()
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
	publicURL := "http://" + listener.Addr().String() + "/"
	if err := instanceLock.writeRegistry(instanceRegistry{
		PID:       os.Getpid(),
		BaseURL:   publicURL,
		Token:     token,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Ready:     true,
	}); err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "sai: starting %s\n", Version)
	fmt.Fprintf(stderr, "sai: server root: %s\n", root)
	fmt.Fprintf(stderr, "sai: root config: %s\n", configPath)
	fmt.Fprintf(stderr, "sai: initial workspace: %s\n", initialCWD)
	fmt.Fprintf(stderr, "sai: web server listening on %s\n", publicURL)
	fmt.Fprintf(stdout, "SAI_WEB_URL\t%s\n", url)
	if !*noOpen {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(stderr, "sai: open browser: %v\n", err)
		} else {
			fmt.Fprintln(stderr, "sai: browser opened")
		}
	} else {
		fmt.Fprintln(stderr, "sai: browser auto-open disabled")
	}
	fmt.Fprintln(stderr, "sai: ready; press Ctrl+C to stop")

	select {
	case <-ctx.Done():
		fmt.Fprintln(stderr, "sai: shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "sai: shutdown: %v\n", err)
			return 1
		}
		fmt.Fprintln(stderr, "sai: stopped")
		return 0
	case err := <-serveDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "sai: web server: %v\n", err)
			return 1
		}
		return 0
	}
}

func resolveStorageRoot(explicit, basename string) (string, error) {
	root := strings.TrimSpace(explicit)
	if root == "" {
		root = strings.TrimSpace(os.Getenv(serverRootEnvName(basename)))
	}
	if root == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("find user config directory: %w", err)
		}
		root = filepath.Join(configDir, basename)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve storage root %q: %w", root, err)
	}
	return filepath.Clean(abs), nil
}

func commandBasename(argv0 string) (string, error) {
	name := strings.TrimSpace(argv0)
	if separator := strings.LastIndexAny(name, `/\`); separator >= 0 {
		name = name[separator+1:]
	}
	if strings.EqualFold(filepath.Ext(name), ".exe") {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." {
		return "", fmt.Errorf("derive command basename from argv[0] %q", argv0)
	}
	return name, nil
}

func serverRootEnvName(basename string) string {
	var normalized strings.Builder
	lastUnderscore := false
	for _, char := range strings.ToUpper(basename) {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			normalized.WriteRune(char)
			lastUnderscore = false
			continue
		}
		if normalized.Len() > 0 && !lastUnderscore {
			normalized.WriteByte('_')
			lastUnderscore = true
		}
	}
	name := strings.Trim(normalized.String(), "_")
	if name == "" {
		return "SAI_SERVER_ROOT"
	}
	return name + "_SERVER_ROOT"
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

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func listen(address string, allowNonLoopback bool) (net.Listener, error) {
	if _, _, err := net.SplitHostPort(strings.TrimSpace(address)); err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	if !isLoopbackAddress(address) && !allowNonLoopback {
		return nil, fmt.Errorf("listen address must use a loopback host (use --allow-non-loopback to bind a non-loopback address)")
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

// openBrowserImpl 是打开默认浏览器的真实实现。
func openBrowserImpl(url string) error {
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

// openBrowser 是可替换的浏览器打开函数，测试可替换为探针。
var openBrowser = openBrowserImpl
