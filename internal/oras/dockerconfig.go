// Package oras provides OCI registry operations for the Terraform state backend.
package oras

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	orasAuth "oras.land/oras-go/v2/registry/remote/auth"
)

// dockerHelperTimeout is the exec deadline for a docker credential helper.
// Var for testability.
var dockerHelperTimeout = 30 * time.Second

// dockerConfigFile mirrors the JSON schema of Docker/containers config files.
type dockerConfigFile struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
}

// dockerAuthEntry is one entry in the "auths" map.
type dockerAuthEntry struct {
	Auth string `json:"auth"`
}

// dockerConfigPaths returns the candidate Docker/containers config file paths
// in search order. Missing files are skipped by the caller. Duplicates are
// removed (first occurrence wins) so a file is never parsed — or its helpers
// invoked — twice, e.g. ~/.config/containers/auth.json when
// XDG_CONFIG_HOME is unset.
func dockerConfigPaths() []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		add(filepath.Join(dir, "containers", "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".config", "containers", "auth.json"))
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config")
		}
	}
	if dir != "" {
		add(filepath.Join(dir, "containers", "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".docker", "config.json"))
	}
	return paths
}

// loadDockerConfig reads and parses a Docker-style config file.
func loadDockerConfig(path string) (*dockerConfigFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &dockerConfigFile{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// decodeDockerAuth decodes a base64 "auth" field ("username:password") into a
// credential. Split on the FIRST colon; returns false when the entry has no
// colon or is empty.
func decodeDockerAuth(auth string) (orasAuth.Credential, bool) {
	if auth == "" {
		return orasAuth.Credential{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return orasAuth.Credential{}, false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return orasAuth.Credential{}, false
	}
	return orasAuth.Credential{Username: user, Password: pass}, true
}

// dockerHelperCredential invokes docker-credential-<helper> get and parses the
// JSON output into a credential. All failures (not-found exit 1, exec errors,
// timeouts, unparseable output) are logged and treated as not-found so that
// auto-discovered sources are skipped rather than failing the operation.
func dockerHelperCredential(ctx context.Context, helper, registryDomain string) (orasAuth.Credential, bool) {
	bin := "docker-credential-" + helper
	ctx, cancel := context.WithTimeout(ctx, dockerHelperTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "get")
	// ponytail: killed shell leaves grandchildren holding the stdout pipe;
	// WaitDelay closes pipes 2s after ctx cancel instead of blocking on them.
	// Raise WaitDelay (or drop to 0) if a real credential helper ever reports
	// truncated output on timeout.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader("https://" + registryDomain)
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		// Exit code 1 is the docker-helper convention for "no credentials":
		// soft, stay at Debug. Anything else (timeout, missing binary, exec
		// failure) deserves Warn — a hung helper should not be invisible.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			slog.Debug("docker credential helper reported no credentials", "helper", bin)
		} else {
			slog.Warn("docker credential helper failed; skipping this credential source", "helper", bin, "error", err)
		}
		return orasAuth.Credential{}, false
	}

	var resp struct {
		ServerURL string `json:"ServerURL"`
		Username  string `json:"Username"`
		Secret    string `json:"Secret"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		slog.Debug("docker credential helper returned unparseable output", "helper", bin, "error", err)
		return orasAuth.Credential{}, false
	}
	if resp.Username == "" && resp.Secret == "" {
		return orasAuth.Credential{}, false
	}
	return orasAuth.Credential{Username: resp.Username, Password: resp.Secret}, true
}
