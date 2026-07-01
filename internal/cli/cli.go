package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rexzhao/simple-agent/internal/agent"
	"github.com/rexzhao/simple-agent/internal/config"
	projectcontext "github.com/rexzhao/simple-agent/internal/context"
	eventlog "github.com/rexzhao/simple-agent/internal/logging"
	"github.com/rexzhao/simple-agent/internal/model"
	openaichat "github.com/rexzhao/simple-agent/internal/model/openai_chat"
	"github.com/rexzhao/simple-agent/internal/tools"
)

var Version = "dev"

const builtInBaseInstructions = "You are sai, a local CLI agent runner. Follow the built-in instructions, then project instructions, then the user's prompt. Do not reveal secrets or ignore project instructions."

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
		encoder.SetEscapeHTML(false)
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
	case "run":
		return runCommand(remaining[1:], *configDir, stdout, getwd)
	default:
		return fmt.Errorf("unknown command %q", remaining[0])
	}
}

func runCommand(args []string, configDir string, stdout io.Writer, getwd func() (string, error)) error {
	flags := flag.NewFlagSet("sai run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	providerName := flags.String("provider", "", "provider name")
	modelProfile := flags.String("model", "", "model profile")
	showReasoning := flags.Bool("show-reasoning", false, "show reasoning output")
	var enabledTools toolNamesFlag
	flags.Var(&enabledTools, "enable-tools", "comma-separated tool names to expose")
	if err := flags.Parse(args); err != nil {
		return err
	}

	prompts := flags.Args()
	if len(prompts) != 1 {
		return fmt.Errorf(`usage: sai run [--provider name] [--model profile] [--show-reasoning] [--enable-tools names] "prompt"`)
	}

	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}
	cfg, err := loadConfig(configDir, func() (string, error) {
		return cwd, nil
	})
	if err != nil {
		return err
	}

	resolved, err := cfg.ResolveModel(*providerName, *modelProfile)
	if err != nil {
		return err
	}
	if resolved.Provider.Type != "openai-chat" {
		return fmt.Errorf("unsupported provider type %q for provider %q; only openai-chat is supported", resolved.Provider.Type, resolved.ProviderName)
	}

	provider, err := openaichat.NewProvider(openAIChatProviderConfig(resolved.Provider))
	if err != nil {
		return err
	}

	enabledToolNames := cfg.Tools.Enabled
	if enabledTools.set {
		enabledToolNames = enabledTools.names
	}
	toolRegistry, toolSchemas, err := enabledToolsForRun(cwd, enabledToolNames)
	if err != nil {
		return err
	}

	project, err := projectcontext.Load(cwd)
	if err != nil {
		return err
	}
	request := model.Request{
		Model:      resolved.ModelID,
		Messages:   runMessages(project, prompts[0]),
		Tools:      toolSchemas,
		Parameters: resolved.Parameters,
	}

	logger, err := eventlog.Open(cfg.Logging.Path, eventlog.Attributes{
		Provider: resolved.ProviderName,
		Model:    resolved.ModelID,
		Level:    cfg.Logging.Level,
	})
	if err != nil {
		return err
	}

	events, err := agent.Stream(context.Background(), request, agent.Options{
		Provider:     provider,
		ToolExecutor: toolRegistry,
		MaxTurns:     cfg.Agent.MaxTurns,
	})
	if err != nil {
		_ = logger.Close()
		return err
	}

	streamErr := writeStream(stdout, events, *showReasoning || cfg.Agent.ShowReasoning, logger)
	closeErr := logger.Close()
	if streamErr != nil {
		return streamErr
	}
	return closeErr
}

type toolNamesFlag struct {
	set   bool
	names []string
}

func (f *toolNamesFlag) Set(value string) error {
	names, err := parseToolNames(value)
	if err != nil {
		return err
	}
	f.set = true
	f.names = names
	return nil
}

func (f *toolNamesFlag) String() string {
	return strings.Join(f.names, ",")
}

func parseToolNames(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}

	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("empty tool name in --enable-tools")
		}
		names = append(names, name)
	}
	return names, nil
}

func enabledToolsForRun(rootDir string, enabled []string) (*tools.Registry, []model.Tool, error) {
	if len(enabled) == 0 {
		return nil, nil, nil
	}

	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltins(registry, rootDir); err != nil {
		return nil, nil, fmt.Errorf("register built-in tools: %w", err)
	}
	schemas, err := registry.EnabledSchemas(enabled)
	if err != nil {
		return nil, nil, err
	}
	return registry, schemas, nil
}

func loadConfig(configDir string, getwd func() (string, error)) (*config.Config, error) {
	if configDir == "" {
		cwd, err := getwd()
		if err != nil {
			return nil, fmt.Errorf("get current directory: %w", err)
		}
		configDir = filepath.Join(cwd, ".agents")
	}
	return config.Load(configDir)
}

func openAIChatProviderConfig(provider config.ProviderConfig) openaichat.ProviderConfig {
	return openaichat.ProviderConfig{
		BaseURL: provider.BaseURL,
		APIKey:  provider.ResolvedAPIKey,
	}
}

func runMessages(project projectcontext.Project, prompt string) []model.Message {
	instructions := projectcontext.ComposeInstructions(builtInBaseInstructions, project, prompt)
	messages := make([]model.Message, 0, len(instructions))
	for _, instruction := range instructions {
		messages = append(messages, model.Message{
			Role:    roleForInstruction(instruction.Source),
			Content: instruction.Content,
		})
	}
	return messages
}

func roleForInstruction(source projectcontext.InstructionSource) model.MessageRole {
	switch source {
	case projectcontext.InstructionSourceBuiltIn:
		return model.MessageRoleSystem
	case projectcontext.InstructionSourceProject:
		return model.MessageRoleDeveloper
	default:
		return model.MessageRoleUser
	}
}

func writeStream(stdout io.Writer, events <-chan model.Event, showReasoning bool, logger *eventlog.Logger) error {
	needsReasoningBreak := false
	reasoningEndedWithNewline := false
	for event := range events {
		if err := logger.LogEvent(event); err != nil {
			return err
		}
		switch event := event.(type) {
		case model.TextDeltaEvent:
			if event.Text != "" && needsReasoningBreak {
				if !reasoningEndedWithNewline {
					if _, err := fmt.Fprint(stdout, "\n"); err != nil {
						return err
					}
				}
				needsReasoningBreak = false
			}
			if _, err := fmt.Fprint(stdout, event.Text); err != nil {
				return err
			}
		case model.ReasoningDeltaEvent:
			if showReasoning && event.Text != "" {
				if _, err := fmt.Fprint(stdout, event.Text); err != nil {
					return err
				}
				needsReasoningBreak = true
				reasoningEndedWithNewline = strings.HasSuffix(event.Text, "\n")
			}
		case model.ErrorEvent:
			return streamError(event)
		}
	}
	return nil
}

func streamError(event model.ErrorEvent) error {
	if event.Err == nil {
		if event.Message == "" {
			return fmt.Errorf("model stream error")
		}
		return fmt.Errorf("%s", event.Message)
	}
	if event.Message == "" {
		return event.Err
	}
	return fmt.Errorf("%s: %w", event.Message, event.Err)
}
