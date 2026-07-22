package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

func WriteProviderConfig(path string, provider ProviderConfig) error {
	path = filepath.Clean(path)
	if err := validateProvider(path, normalizeProvider(provider, filepath.Dir(path))); err != nil {
		return err
	}
	data, err := yaml.Marshal(provider)
	if err != nil {
		return fmt.Errorf("encode provider file %q: %w", path, err)
	}
	if err := writeConfigFileAtomic(path, data); err != nil {
		return fmt.Errorf("write provider file %q: %w", path, err)
	}
	return nil
}

func UpdateDefaultModel(configPath, provider, model string) error {
	configPath = filepath.Clean(configPath)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", configPath, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse config file %q: %w", configPath, err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config file %q must contain a YAML mapping", configPath)
	}
	setYAMLScalar(document.Content[0], "default_provider", provider)
	setYAMLScalar(document.Content[0], "default_model", model)
	updated, err := yaml.Marshal(&document)
	if err != nil {
		return fmt.Errorf("encode config file %q: %w", configPath, err)
	}
	if err := writeConfigFileAtomic(configPath, updated); err != nil {
		return fmt.Errorf("write config file %q: %w", configPath, err)
	}
	return nil
}

func setYAMLScalar(mapping *yaml.Node, key, value string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1].Kind = yaml.ScalarNode
			mapping.Content[index+1].Tag = "!!str"
			mapping.Content[index+1].Value = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func writeConfigFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat config file: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary config file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary config file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary config file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary config file: %w", err)
	}
	if err := replaceConfigFile(tempPath, path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	cleanup = false
	return nil
}

func replaceConfigFile(source, target string) error {
	delays := [...]time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		err := os.Rename(source, target)
		if err == nil {
			return nil
		}
		if runtime.GOOS != "windows" || attempt >= len(delays) || (!errors.Is(err, syscall.Errno(5)) && !errors.Is(err, syscall.Errno(32))) {
			return err
		}
		time.Sleep(delays[attempt])
	}
}
