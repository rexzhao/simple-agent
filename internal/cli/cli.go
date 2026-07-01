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

	"github.com/rexzhao/simple-agent/internal/config"
	projectcontext "github.com/rexzhao/simple-agent/internal/context"
	"github.com/rexzhao/simple-agent/internal/model"
	openaichat "github.com/rexzhao/simple-agent/internal/model/openai_chat"
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
	if err := flags.Parse(args); err != nil {
		return err
	}

	prompts := flags.Args()
	if len(prompts) != 1 {
		return fmt.Errorf(`usage: sai run [--provider name] [--model profile] [--show-reasoning] "prompt"`)
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

	project, err := projectcontext.Load(cwd)
	if err != nil {
		return err
	}
	request := model.Request{
		Model:      resolved.ModelID,
		Messages:   runMessages(project, prompts[0]),
		Parameters: resolved.Parameters,
	}

	events, err := provider.Stream(context.Background(), request)
	if err != nil {
		return err
	}
	return writeStream(stdout, events, *showReasoning || cfg.Agent.ShowReasoning)
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

func writeStream(stdout io.Writer, events <-chan model.Event, showReasoning bool) error {
	needsReasoningBreak := false
	reasoningEndedWithNewline := false
	for event := range events {
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
