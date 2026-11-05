// Package install registers the MCP server and UserPromptSubmit hook, and
// installs the per-user daemon service.
package install

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Options controls what Install sets up.
type Options struct {
	Project   bool // register MCP in the current repo's shared .mcp.json
	NoService bool // skip the daemon autostart unit
}

func userClaudeSettings() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Install is idempotent: running it repeatedly leaves one MCP entry and one hook.
func Install(opts Options) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	scope := "user"
	if opts.Project {
		scope = "project"
	}
	args := []string{"mcp", "add", "-s", scope, "teamconnect", "--", exe, "mcp"}
	cmd := exec.Command("claude", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("注册 MCP 失败（请确认 claude 在 PATH 中）：%w", err)
	}
	fmt.Printf("MCP 已注册（scope=%s）：teamconnect -> %s mcp\n", scope, exe)

	if err := installHook(exe); err != nil {
		return fmt.Errorf("安装 UserPromptSubmit hook 失败：%w", err)
	}
	fmt.Println("UserPromptSubmit hook 已安装")

	if !opts.NoService {
		if err := installService(exe); err != nil {
			fmt.Printf("警告：daemon 自启动未配置：%v\n", err)
		} else {
			fmt.Println("daemon 自启动已配置")
		}
	}
	return nil
}

func installHook(exe string) error {
	path, err := userClaudeSettings()
	if err != nil {
		return err
	}
	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("现有 settings.json 不是合法 JSON：%w", err)
		}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	command := fmt.Sprintf("%q hook-prompt-submit", exe)
	entries, _ := hooks["UserPromptSubmit"].([]any)
	found := false
	for _, e := range entries {
		m, _ := e.(map[string]any)
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); cmd == command {
				found = true
			}
		}
	}
	if !found {
		entries = append(entries, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		})
		hooks["UserPromptSubmit"] = entries
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func installService(exe string) error {
	switch runtime.GOOS {
	case "linux":
		return installSystemdUser(exe)
	case "windows":
		fmt.Println("Windows 自启动：请在登录时运行 `schtasks /create /sc onlogon /tn TeamConnectDaemon /tr \"\\\"" + exe + "\\\" daemon\"`")
		return nil
	default:
		return fmt.Errorf("当前平台 %s 未实现自启动", runtime.GOOS)
	}
}

func installSystemdUser(exe string) error {
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	unitDir := filepath.Join(dir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unit := strings.Join([]string{
		"[Unit]",
		"Description=TeamConnect local daemon",
		"After=network.target",
		"",
		"[Service]",
		"ExecStart=" + exe + " daemon",
		"Restart=on-failure",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(unitDir, "teamconnect-daemon.service"), []byte(unit), 0o644); err != nil {
		return err
	}
	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemd 不可用，已写入 unit，请手动启用：%w", err)
	}
	if err := exec.Command("systemctl", "--user", "enable", "--now", "teamconnect-daemon.service").Run(); err != nil {
		return fmt.Errorf("已写入 unit，但启用失败，请手动执行 systemctl --user enable --now teamconnect-daemon：%w", err)
	}
	return nil
}

// PromptPassword reads a password from the terminal without echoing it.
func PromptPassword(prompt string) (string, error) {
	return promptPassword(prompt)
}
