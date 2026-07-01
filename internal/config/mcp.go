package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type MCPServerConfig struct {
	ID      string            `json:"id" yaml:"id"`
	Enabled bool              `json:"enabled" yaml:"enabled"`
	Command string            `json:"command" yaml:"command"`
	Args    []string          `json:"args" yaml:"args"`
	Env     map[string]string `json:"env" yaml:"env"`
}

func (s *MCPServerConfig) UnmarshalYAML(value *yaml.Node) error {
	type serverYAML struct {
		ID      string            `yaml:"id"`
		Enabled *bool             `yaml:"enabled"`
		Command string            `yaml:"command"`
		Args    []string          `yaml:"args"`
		Env     map[string]string `yaml:"env"`
	}

	var raw serverYAML
	if err := value.Decode(&raw); err != nil {
		return err
	}

	enabled := true
	if raw.Enabled != nil {
		enabled = *raw.Enabled
	}

	s.ID = raw.ID
	s.Enabled = enabled
	s.Command = raw.Command
	s.Args = raw.Args
	s.Env = raw.Env
	return nil
}

func (s MCPServerConfig) MarshalJSON() ([]byte, error) {
	type serverJSON struct {
		ID      string            `json:"id"`
		Enabled bool              `json:"enabled"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}

	env := make(map[string]string, len(s.Env))
	for name, value := range s.Env {
		env[name] = redactedSecretValue(value)
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(serverJSON{
		ID:      s.ID,
		Enabled: s.Enabled,
		Command: s.Command,
		Args:    s.Args,
		Env:     env,
	}); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func (c *Config) MCPServerList() []MCPServerConfig {
	ids := make([]string, 0, len(c.MCPServers))
	for id := range c.MCPServers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	servers := make([]MCPServerConfig, 0, len(ids))
	for _, id := range ids {
		servers = append(servers, c.MCPServers[id])
	}
	return servers
}

func (c *Config) SelectedMCPServers(overrideIDs []string, useOverride bool) ([]MCPServerConfig, error) {
	if useOverride {
		return c.mcpServersByID(overrideIDs)
	}

	servers := c.MCPServerList()
	selected := make([]MCPServerConfig, 0, len(servers))
	for _, server := range servers {
		if server.Enabled {
			selected = append(selected, server)
		}
	}
	return selected, nil
}

func (c *Config) mcpServersByID(ids []string) ([]MCPServerConfig, error) {
	selected := make([]MCPServerConfig, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("empty MCP server id")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate MCP server id %q in --enable-mcp", id)
		}
		server, ok := c.MCPServers[id]
		if !ok {
			return nil, fmt.Errorf("unknown MCP server %q; available MCP servers: %s", id, c.formatMCPServerChoices())
		}
		seen[id] = struct{}{}
		selected = append(selected, server)
	}
	return selected, nil
}

func (c *Config) formatMCPServerChoices() string {
	servers := c.MCPServerList()
	if len(servers) == 0 {
		return "(none)"
	}

	ids := make([]string, 0, len(servers))
	for _, server := range servers {
		ids = append(ids, server.ID)
	}
	return strings.Join(ids, ", ")
}

func loadMCPServers(mcpDir string) (map[string]MCPServerConfig, error) {
	servers := make(map[string]MCPServerConfig)
	if strings.TrimSpace(mcpDir) == "" {
		return servers, nil
	}

	entries, err := os.ReadDir(mcpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return servers, nil
		}
		return nil, fmt.Errorf("read mcp_dir %q: %w", mcpDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !isYAMLFile(entry.Name()) {
			continue
		}

		path := filepath.Join(mcpDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read MCP server file %q: %w", path, err)
		}

		var server MCPServerConfig
		if err := yaml.Unmarshal(data, &server); err != nil {
			return nil, fmt.Errorf("parse MCP server file %q: %w", path, err)
		}
		normalizeMCPServer(&server)
		if err := validateMCPServer(path, server); err != nil {
			return nil, err
		}
		if _, exists := servers[server.ID]; exists {
			return nil, fmt.Errorf("duplicate MCP server id %q", server.ID)
		}
		servers[server.ID] = server
	}
	return servers, nil
}

func normalizeMCPServer(server *MCPServerConfig) {
	server.ID = strings.TrimSpace(server.ID)
	server.Command = strings.TrimSpace(server.Command)
	if server.Args == nil {
		server.Args = []string{}
	}
	if server.Env == nil {
		server.Env = map[string]string{}
	}
}

func validateMCPServer(path string, server MCPServerConfig) error {
	if server.ID == "" {
		return fmt.Errorf("MCP server file %q is missing id", path)
	}
	if server.Command == "" {
		return fmt.Errorf("MCP server file %q server %q is missing command", path, server.ID)
	}
	return nil
}
