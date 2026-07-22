package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	defaultToolOutputMaxLines = 2000
	defaultToolOutputMaxBytes = 50 * 1024
)

type toolOutputTruncation struct {
	content     string
	totalLines  int
	totalBytes  int
	outputLines int
	outputBytes int
	direction   string
	limit       string
}

func limitToolResultOutput(result model.ToolResult) model.ToolResult {
	truncation, truncated := truncateToolOutput(result.Content, result.Name == "shell")
	if !truncated {
		return result
	}
	result.Content = truncation.content + "\n\n" + fmt.Sprintf(
		"[Tool output truncated: showing %s %d of %d lines (%s of %s); reached the %s limit.]",
		truncation.direction,
		truncation.outputLines,
		truncation.totalLines,
		formatToolOutputSize(truncation.outputBytes),
		formatToolOutputSize(truncation.totalBytes),
		truncation.limit,
	)
	return result
}

func truncateToolOutput(content string, keepTail bool) (toolOutputTruncation, bool) {
	totalBytes := len([]byte(content))
	lines := splitToolOutputLines(content)
	totalLines := len(lines)
	if totalLines <= defaultToolOutputMaxLines && totalBytes <= defaultToolOutputMaxBytes {
		return toolOutputTruncation{}, false
	}

	if keepTail {
		selected, limit := truncateToolOutputTail(lines)
		output := strings.Join(selected, "\n")
		return toolOutputTruncation{
			content:     output,
			totalLines:  totalLines,
			totalBytes:  totalBytes,
			outputLines: len(selected),
			outputBytes: len([]byte(output)),
			direction:   "last",
			limit:       limit,
		}, true
	}

	selected, limit := truncateToolOutputHead(lines)
	output := strings.Join(selected, "\n")
	return toolOutputTruncation{
		content:     output,
		totalLines:  totalLines,
		totalBytes:  totalBytes,
		outputLines: len(selected),
		outputBytes: len([]byte(output)),
		direction:   "first",
		limit:       limit,
	}, true
}

func splitToolOutputLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func truncateToolOutputHead(lines []string) ([]string, string) {
	selected := make([]string, 0, min(len(lines), defaultToolOutputMaxLines))
	outputBytes := 0
	for _, line := range lines {
		if len(selected) == defaultToolOutputMaxLines {
			return selected, fmt.Sprintf("%d line", defaultToolOutputMaxLines)
		}
		separatorBytes := 0
		if len(selected) > 0 {
			separatorBytes = 1
		}
		lineBytes := len([]byte(line))
		if outputBytes+separatorBytes+lineBytes > defaultToolOutputMaxBytes {
			if len(selected) == 0 {
				selected = append(selected, truncateUTF8Prefix(line, defaultToolOutputMaxBytes))
			}
			return selected, formatToolOutputSize(defaultToolOutputMaxBytes)
		}
		selected = append(selected, line)
		outputBytes += separatorBytes + lineBytes
	}
	return selected, formatToolOutputSize(defaultToolOutputMaxBytes)
}

func truncateToolOutputTail(lines []string) ([]string, string) {
	selected := make([]string, 0, min(len(lines), defaultToolOutputMaxLines))
	outputBytes := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if len(selected) == defaultToolOutputMaxLines {
			return selected, fmt.Sprintf("%d line", defaultToolOutputMaxLines)
		}
		separatorBytes := 0
		if len(selected) > 0 {
			separatorBytes = 1
		}
		lineBytes := len([]byte(lines[i]))
		if outputBytes+separatorBytes+lineBytes > defaultToolOutputMaxBytes {
			if len(selected) == 0 {
				selected = append(selected, truncateUTF8Suffix(lines[i], defaultToolOutputMaxBytes))
			}
			return selected, formatToolOutputSize(defaultToolOutputMaxBytes)
		}
		selected = append([]string{lines[i]}, selected...)
		outputBytes += separatorBytes + lineBytes
	}
	return selected, formatToolOutputSize(defaultToolOutputMaxBytes)
}

func truncateUTF8Prefix(content string, maxBytes int) string {
	if len(content) <= maxBytes {
		return content
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	return content[:end]
}

func truncateUTF8Suffix(content string, maxBytes int) string {
	if len(content) <= maxBytes {
		return content
	}
	start := len(content) - maxBytes
	for start < len(content) && !utf8.RuneStart(content[start]) {
		start++
	}
	return content[start:]
}

func formatToolOutputSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}
