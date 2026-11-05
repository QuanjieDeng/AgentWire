package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallHookIsIdempotentAndPreservesSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settingsPath), 0o700)
	os.WriteFile(settingsPath, []byte(`{
	  "theme": "dark",
	  "hooks": {"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "existing-hook"}]}]}
	}`), 0o600)

	for i := 0; i < 2; i++ {
		if err := installHook("/usr/local/bin/agentwire"); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s["theme"] != "dark" {
		t.Fatalf("unrelated setting was lost: %+v", s)
	}
	hooks := s["hooks"].(map[string]any)
	entries := hooks["UserPromptSubmit"].([]any)
	commands := map[string]int{}
	for _, e := range entries {
		m := e.(map[string]any)
		for _, h := range m["hooks"].([]any) {
			hm := h.(map[string]any)
			commands[hm["command"].(string)]++
		}
	}
	if commands["existing-hook"] != 1 {
		t.Fatalf("existing hook changed: %+v", commands)
	}
	want := `"/usr/local/bin/agentwire" hook-prompt-submit`
	if commands[want] != 1 {
		t.Fatalf("agentwire hook should appear exactly once, got %+v", commands)
	}
}
