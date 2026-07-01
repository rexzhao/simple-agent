package projectcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const AgentsFileName = "AGENTS.md"

type Project struct {
	Directory        string
	Instructions     string
	InstructionsPath string
	HasInstructions  bool
}

type InstructionSource string

const (
	InstructionSourceBuiltIn InstructionSource = "sai_builtin"
	InstructionSourceProject InstructionSource = "agents_md"
	InstructionSourceUser    InstructionSource = "user_prompt"
)

type InstructionPriority int

const (
	PriorityBuiltInBase InstructionPriority = iota
	PriorityProject
	PriorityUser
)

type Instruction struct {
	Source   InstructionSource
	Priority InstructionPriority
	Content  string
}

func Load(projectDir string) (Project, error) {
	if strings.TrimSpace(projectDir) == "" {
		return Project{}, fmt.Errorf("project directory is required")
	}

	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project directory: %w", err)
	}
	absProjectDir = filepath.Clean(absProjectDir)
	agentsPath := filepath.Join(absProjectDir, AgentsFileName)

	data, err := os.ReadFile(agentsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Project{
				Directory:        absProjectDir,
				InstructionsPath: agentsPath,
			}, nil
		}
		return Project{}, fmt.Errorf("read %s: %w", AgentsFileName, err)
	}

	return Project{
		Directory:        absProjectDir,
		Instructions:     string(data),
		InstructionsPath: agentsPath,
		HasInstructions:  true,
	}, nil
}

func ComposeInstructions(builtInBase string, project Project, userPrompt string) []Instruction {
	instructions := []Instruction{
		{
			Source:   InstructionSourceBuiltIn,
			Priority: PriorityBuiltInBase,
			Content:  builtInBase,
		},
	}

	if project.HasInstructions {
		instructions = append(instructions, Instruction{
			Source:   InstructionSourceProject,
			Priority: PriorityProject,
			Content:  project.Instructions,
		})
	}

	instructions = append(instructions, Instruction{
		Source:   InstructionSourceUser,
		Priority: PriorityUser,
		Content:  userPrompt,
	})

	return instructions
}
