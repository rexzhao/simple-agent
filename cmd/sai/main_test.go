package main

import (
	"os"
	"testing"
)

func TestAppNameFromArgv0(t *testing.T) {
	original := os.Args
	defer func() { os.Args = original }()
	for argv0, want := range map[string]string{
		"sai":                        "sai",
		"sai.exe":                    "sai",
		"dist/sai-windows-amd64.exe": "sai",
		`F:\bin\sai.exe`:             "sai",
		"myagent-v2.exe":             "myagent",
		"plain":                      "plain",
	} {
		os.Args = []string{argv0}
		if got := appName(); got != want {
			t.Errorf("appName(%q) = %q, want %q", argv0, got, want)
		}
	}
	// A leading dash keeps the whole name: the part before it is empty.
	os.Args = []string{"-weird.exe"}
	if got := appName(); got != "-weird" {
		t.Errorf("appName with leading dash = %q", got)
	}
}
