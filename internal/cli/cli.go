package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/rexzhao/simple-agent/internal/config"
)

var Version = "dev"

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithGetwd(args, stdout, stderr, os.Getwd)
}

func RunWithGetwd(args []string, stdout, stderr io.Writer, getwd func() (string, error)) int {
	if err := execute(args, stdout, getwd); err != nil {
		fmt.Fprintf(stderr, "sai: %v\n", err)
		return 1
	}
	return 0
}

func execute(args []string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configDir := flags.String("config-dir", "", "configuration directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	remaining := flags.Args()
	if len(remaining) == 0 {
		return fmt.Errorf("missing command")
	}

	switch remaining[0] {
	case "version":
		if len(remaining) != 1 {
			return fmt.Errorf("usage: sai version")
		}
		fmt.Fprintf(stdout, "sai %s\n", Version)
		return nil
	case "config":
		if len(remaining) != 2 || remaining[1] != "show" {
			return fmt.Errorf("usage: sai config show")
		}
		cfg, err := loadConfig(*configDir, getwd)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(cfg)
	case "models":
		if len(remaining) != 2 || remaining[1] != "list" {
			return fmt.Errorf("usage: sai models list")
		}
		cfg, err := loadConfig(*configDir, getwd)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, "PROVIDER\tPROFILE\tMODEL ID")
		for _, model := range cfg.ModelList() {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", model.Provider, model.Profile, model.ID)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", remaining[0])
	}
}

func loadConfig(configDir string, getwd func() (string, error)) (*config.Config, error) {
	if configDir == "" {
		cwd, err := getwd()
		if err != nil {
			return nil, fmt.Errorf("get current directory: %w", err)
		}
		configDir = cwd
	}
	return config.Load(configDir)
}
