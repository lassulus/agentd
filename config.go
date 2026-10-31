package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Harness struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
}

type Config struct {
	Listen        string             `json:"listen"`
	StateDir      string             `json:"stateDir"`
	WorkspaceRoot string             `json:"workspaceRoot"`
	Harnesses     map[string]Harness `json:"harnesses"`
	Auth          struct {
		OIDC *struct {
			Issuer   string `json:"issuer"`
			Audience string `json:"audience"`
		} `json:"oidc"`
		TokensFile string `json:"tokensFile"`
	} `json:"auth"`
	// Users allowed to log in with OAuth2 tokens: a token's `sub`, or its
	// email when the token says `email_verified`.
	Users []string `json:"users"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8790"
	}
	if c.StateDir == "" || c.WorkspaceRoot == "" {
		return nil, fmt.Errorf("%s: stateDir and workspaceRoot are required", path)
	}
	for name, h := range c.Harnesses {
		if len(h.Command) == 0 {
			return nil, fmt.Errorf("%s: harness %q has no command", path, name)
		}
	}
	root, err := filepath.EvalSymlinks(c.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("workspaceRoot: %w", err)
	}
	c.WorkspaceRoot = root
	return &c, nil
}
