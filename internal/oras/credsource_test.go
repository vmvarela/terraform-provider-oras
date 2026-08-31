// Package oras provides OCI registry operations for the Terraform state backend.
package oras

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	orasAuth "oras.land/oras-go/v2/registry/remote/auth"
)

// clearCredEnv clears all environment variables that influence credential
// resolution so tests are hermetic.
func clearCredEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ORAS_TOKEN", "GHCR_TOKEN", "GITHUB_TOKEN",
		"TF_CLI_CONFIG_FILE", "TERRAFORM_CONFIG",
		"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME",
	} {
		t.Setenv(key, "")
	}
	tmpHome := t.TempDir()
	// os.UserHomeDir() uses HOME on Unix but USERPROFILE on Windows.
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
}

// writeTestFile writes content to dir/name and returns the full path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeHelperScript writes an executable shell script into a temp dir and
// prepends the dir to PATH.
func writeHelperScript(t *testing.T, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-based credential helper test requires a POSIX shell")
	}
	dir := t.TempDir()
	writeTestFile(t, dir, name, "#!/bin/sh\n"+script)
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestConfigKeyMatch(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want int
	}{
		{"exact domain", "ghcr.io", 2},
		{"global key", "", 1},
		{"domain matches", "ghcr.io", 2},
		{"path prefix match", "ghcr.io/org", 3},
		{"full path match", "ghcr.io/org/app", 4},
		{"segment boundary: orgx does not match org", "ghcr.io/orgx", 0},
		{"key path longer than want path", "ghcr.io/org/app/extra", 0},
		{"wrong domain", "registry.example.com", 0},
		{"path mismatch", "ghcr.io/other/app", 0},
		{"tag key rejected", "ghcr.io/org:latest", 0},
		{"digest key rejected", "ghcr.io/org/app@sha256:1234", 0},
		{"invalid key", "ghcr.io/:tag", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configKeyMatch(tt.key, "ghcr.io", "org/app"); got != tt.want {
				t.Errorf("configKeyMatch(%q) = %d, want %d", tt.key, got, tt.want)
			}
		})
	}

	t.Run("bare domain key has empty path", func(t *testing.T) {
		if got := configKeyMatch("ghcr.io", "ghcr.io", ""); got != 2 {
			t.Errorf("bare domain vs empty path = %d, want 2", got)
		}
	})
	t.Run("domain key matches any path with lower specificity than path key", func(t *testing.T) {
		if configKeyMatch("ghcr.io", "ghcr.io", "org/app") >= configKeyMatch("ghcr.io/org", "ghcr.io", "org/app") {
			t.Error("path key should be more specific than domain key")
		}
	})
}

func TestDecodeDockerAuth(t *testing.T) {
	tests := []struct {
		name      string
		auth      string
		wantOK    bool
		wantCred  orasAuth.Credential
	}{
		{
			name:     "password containing colon",
			auth:     base64.StdEncoding.EncodeToString([]byte("user:pass:word")),
			wantOK:   true,
			wantCred: orasAuth.Credential{Username: "user", Password: "pass:word"},
		},
		{
			name:    "no colon",
			auth:    base64.StdEncoding.EncodeToString([]byte("justauser")),
			wantOK:  false,
		},
		{
			name:   "empty auth",
			auth:   "",
			wantOK: false,
		},
		{
			name:   "invalid base64",
			auth:   "!!!not-base64!!!",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cred, ok := decodeDockerAuth(tt.auth)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && cred != tt.wantCred {
				t.Errorf("cred = %+v, want %+v", cred, tt.wantCred)
			}
		})
	}
}

func TestResolveConfiguredCredential_DockerConfig(t *testing.T) {
	clearCredEnv(t)

	t.Run("auths entry matched via XDG_RUNTIME_DIR", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		writeTestFile(t, dir, "containers/auth.json", `{
			"auths": {"ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("u1:p1")) + `"}}
		}`)

		cred, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app")
		if !ok || cred.Username != "u1" || cred.Password != "p1" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("auths entry via XDG_CONFIG_HOME", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		writeTestFile(t, dir, "containers/auth.json", `{
			"auths": {"ghcr.io/org": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("u2:p2")) + `"}}
		}`)

		cred, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app")
		if !ok || cred.Username != "u2" || cred.Password != "p2" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
		// Path-scoped key must not match a sibling path.
		if _, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "other/app"); ok {
			t.Error("ghcr.io/org key matched other/app, want no match")
		}
	})

	t.Run("empty auth skipped", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		writeTestFile(t, dir, ".docker/config.json", `{
			"auths": {"ghcr.io": {"auth": ""}}
		}`)

		if _, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app"); ok {
			t.Error("empty auth entry matched, want skip")
		}
	})

	t.Run("no config files", func(t *testing.T) {
		if _, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app"); ok {
			t.Error("matched credential without any config, want not-found")
		}
	})

	t.Run("credHelpers", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		writeTestFile(t, dir, ".docker/config.json", `{
			"credHelpers": {"ghcr.io": "testhelper"}
		}`)
		writeHelperScript(t, "docker-credential-testhelper", `echo '{"ServerURL":"https://ghcr.io","Username":"hu","Secret":"hp"}'`)

		cred, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app")
		if !ok || cred.Username != "hu" || cred.Password != "hp" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("credsStore", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		writeTestFile(t, dir, ".docker/config.json", `{"credsStore": "teststore"}`)
		writeHelperScript(t, "docker-credential-teststore", `echo '{"ServerURL":"https://ghcr.io","Username":"su","Secret":"sp"}'`)

		cred, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app")
		if !ok || cred.Username != "su" || cred.Password != "sp" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("search order: XDG_RUNTIME_DIR wins over HOME docker config", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", dir)
		t.Setenv("HOME", home)
		writeTestFile(t, dir, "containers/auth.json", `{
			"auths": {"ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("runtime:rt")) + `"}}
		}`)
		writeTestFile(t, home, ".docker/config.json", `{
			"auths": {"ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("docker:dc")) + `"}}
		}`)

		cred, ok := resolveConfiguredCredential(context.Background(), "ghcr.io", "org/app")
		if !ok || cred.Username != "runtime" {
			t.Errorf("got ok=%v cred=%+v, want runtime (first source wins on tie)", ok, cred)
		}
	})
}

func TestDockerHelperCredential(t *testing.T) {
	clearCredEnv(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-based credential helper test requires a POSIX shell")
	}

	t.Run("success", func(t *testing.T) {
		writeHelperScript(t, "docker-credential-ok", `echo '{"ServerURL":"https://ghcr.io","Username":"hu","Secret":"hp"}'`)
		cred, ok := dockerHelperCredential(context.Background(), "ok", "ghcr.io")
		if !ok || cred.Username != "hu" || cred.Password != "hp" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("exit 1 means not found", func(t *testing.T) {
		writeHelperScript(t, "docker-credential-notfound", `exit 1`)
		if _, ok := dockerHelperCredential(context.Background(), "notfound", "ghcr.io"); ok {
			t.Error("exit 1 should be treated as not-found")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		orig := dockerHelperTimeout
		dockerHelperTimeout = 100 * time.Millisecond
		t.Cleanup(func() { dockerHelperTimeout = orig })

		writeHelperScript(t, "docker-credential-slow", `sleep 5`)
		start := time.Now()
		if _, ok := dockerHelperCredential(context.Background(), "slow", "ghcr.io"); ok {
			t.Error("timed-out helper should be treated as not-found")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("helper took %v, context timeout was not applied", elapsed)
		}
	})

	t.Run("missing helper binary", func(t *testing.T) {
		if _, ok := dockerHelperCredential(context.Background(), "doesnotexist-xyz", "ghcr.io"); ok {
			t.Error("missing helper should be treated as not-found")
		}
	})
}

func TestLoadCLIConfig(t *testing.T) {
	clearCredEnv(t)

	t.Run("basic credentials", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_credentials "ghcr.io" {
				username = "cu"
				password = "cp"
			}
		`)
		cfg, err := loadCLIConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.Credentials) != 1 || cfg.Credentials[0].ConfigKey != "ghcr.io" {
			t.Fatalf("unexpected blocks: %+v", cfg.Credentials)
		}
		cred, ok := cliBlockCredential(context.Background(), cfg.Credentials[0], "ghcr.io")
		if !ok || cred.Username != "cu" || cred.Password != "cp" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("oauth credentials", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_credentials "ghcr.io/org" {
				access_token  = "at"
				refresh_token = "rt"
			}
		`)
		cfg, err := loadCLIConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cred, ok := cliBlockCredential(context.Background(), cfg.Credentials[0], "ghcr.io")
		if !ok || cred.AccessToken != "at" || cred.RefreshToken != "rt" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("helper credentials", func(t *testing.T) {
		writeHelperScript(t, "docker-credential-cli", `echo '{"ServerURL":"https://ghcr.io","Username":"hu","Secret":"hp"}'`)
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_credentials "ghcr.io" {
				docker_credentials_helper = "cli"
			}
		`)
		cfg, err := loadCLIConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cred, ok := cliBlockCredential(context.Background(), cfg.Credentials[0], "ghcr.io")
		if !ok || cred.Username != "hu" || cred.Password != "hp" {
			t.Errorf("got ok=%v cred=%+v", ok, cred)
		}
	})

	t.Run("default credentials block", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_default_credentials {
				docker_credentials_helper = "desktop"
			}
		`)
		cfg, err := loadCLIConfig(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DefaultProvider == nil || cfg.DefaultProvider.DockerCredentialsHelper != "desktop" {
			t.Fatalf("unexpected default block: %+v", cfg.DefaultProvider)
		}
	})

	t.Run("missing file returns nil config, no error", func(t *testing.T) {
		cfg, err := loadCLIConfig(filepath.Join(t.TempDir(), "nonexistent.tfrc"))
		if err != nil || cfg != nil {
			t.Errorf("got cfg=%v err=%v, want nil/nil", cfg, err)
		}
	})

	t.Run("invalid block: no credential group", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_credentials "ghcr.io" {
			}
		`)
		if _, err := loadCLIConfig(path); err == nil {
			t.Error("expected error for empty oci_credentials block")
		}
	})

	t.Run("invalid block: username without password", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_credentials "ghcr.io" {
				username = "u"
			}
		`)
		if _, err := loadCLIConfig(path); err == nil {
			t.Error("expected error for username-only block")
		}
	})

	t.Run("invalid block: two credential groups", func(t *testing.T) {
		dir := t.TempDir()
		path := writeTestFile(t, dir, ".terraformrc", `
			oci_credentials "ghcr.io" {
				username = "u"
				password = "p"
				access_token = "t"
			}
		`)
		if _, err := loadCLIConfig(path); err == nil {
			t.Error("expected error for block with basic + oauth groups")
		}
	})
}

func TestResolveCredentialsPrecedence(t *testing.T) {
	clearCredEnv(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-based credential helper test requires a POSIX shell")
	}

	credOf := func(fn orasAuth.CredentialFunc) orasAuth.Credential {
		cred, err := fn(context.Background(), "ghcr.io")
		if err != nil {
			t.Fatalf("credential func error: %v", err)
		}
		return cred
	}

	// Set up CLI config + docker config used by later subtests.
	dir := t.TempDir()
	tfrc := writeTestFile(t, dir, ".terraformrc", `
		oci_credentials "ghcr.io" {
			username = "cli-user"
			password = "cli-pass"
		}
	`)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestFile(t, home, ".docker/config.json", `{
		"auths": {"ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("docker-user:docker-pass")) + `"}}
	}`)

	t.Run("token beats everything", func(t *testing.T) {
		t.Setenv("ORAS_TOKEN", "env-token")
		t.Setenv("TF_CLI_CONFIG_FILE", tfrc)
		fn, token, _ := resolveCredentials("ghcr.io", "org/app", Config{Token: "explicit"})
		if token != "explicit" || credOf(fn).AccessToken != "explicit" {
			t.Errorf("token priority broken: token=%q cred=%+v", token, credOf(fn))
		}
	})

	t.Run("env beats CLI config", func(t *testing.T) {
		t.Setenv("ORAS_TOKEN", "env-token")
		t.Setenv("TF_CLI_CONFIG_FILE", tfrc)
		fn, token, _ := resolveCredentials("ghcr.io", "org/app", Config{})
		if token != "env-token" || credOf(fn).AccessToken != "env-token" {
			t.Errorf("env priority broken: token=%q cred=%+v", token, credOf(fn))
		}
	})

	t.Run("CLI config beats docker config", func(t *testing.T) {
		t.Setenv("TF_CLI_CONFIG_FILE", tfrc)
		fn, token, _ := resolveCredentials("ghcr.io", "org/app", Config{})
		if token != "" {
			t.Errorf("configured credential should not set resolved token, got %q", token)
		}
		cred := credOf(fn)
		if cred.Username != "cli-user" || cred.Password != "cli-pass" {
			t.Errorf("got %+v, want CLI config credential", cred)
		}
	})

	t.Run("docker config used when no CLI config", func(t *testing.T) {
		fn, token, _ := resolveCredentials("ghcr.io", "org/app", Config{})
		if token != "" {
			t.Errorf("docker config should not set resolved token, got %q", token)
		}
		cred := credOf(fn)
		if cred.Username != "docker-user" || cred.Password != "docker-pass" {
			t.Errorf("got %+v, want docker config credential", cred)
		}
	})

	t.Run("anonymous when nothing configured", func(t *testing.T) {
		t.Setenv("TF_CLI_CONFIG_FILE", "")
		t.Setenv("TERRAFORM_CONFIG", "")
		t.Setenv("HOME", t.TempDir()) // no docker config, no terraformrc
		fn, token, cred := resolveCredentials("registry.example.com", "some/repo", Config{})
		if token != "" {
			t.Errorf("token = %q, want empty", token)
		}
		got, err := fn(context.Background(), "registry.example.com")
		if err != nil {
			t.Fatalf("credential func error: %v", err)
		}
		if got != orasAuth.EmptyCredential || cred != orasAuth.EmptyCredential {
			t.Errorf("cred = %+v resolved = %+v, want EmptyCredential", cred, got)
		}
	})
}

func TestAccessTokenFallbackFromResolvedCredential(t *testing.T) {
	t.Run("explicit token wins", func(t *testing.T) {
		r := &orasRepositoryClient{
			token:        "explicit",
			resolvedCred: orasAuth.Credential{AccessToken: "from-cred"},
		}
		token, err := r.accessToken(context.Background())
		if err != nil || token != "explicit" {
			t.Errorf("got %q err=%v, want explicit", token, err)
		}
	})

	t.Run("resolved AccessToken", func(t *testing.T) {
		r := &orasRepositoryClient{
			resolvedCred: orasAuth.Credential{AccessToken: "at", Password: "pw"},
		}
		token, err := r.accessToken(context.Background())
		if err != nil || token != "at" {
			t.Errorf("got %q err=%v, want at", token, err)
		}
	})

	t.Run("resolved Password as fallback", func(t *testing.T) {
		r := &orasRepositoryClient{
			resolvedCred: orasAuth.Credential{Username: "u", Password: "pw"},
		}
		token, err := r.accessToken(context.Background())
		if err != nil || token != "pw" {
			t.Errorf("got %q err=%v, want pw", token, err)
		}
	})

	t.Run("no credential yields empty string", func(t *testing.T) {
		r := &orasRepositoryClient{resolvedCred: orasAuth.EmptyCredential}
		token, err := r.accessToken(context.Background())
		if err != nil || token != "" {
			t.Errorf("got %q err=%v, want empty", token, err)
		}
	})

	t.Run("resolveCredentials returns configured credential", func(t *testing.T) {
		clearCredEnv(t)
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		t.Setenv("USERPROFILE", dir) // os.UserHomeDir() on Windows ignores HOME
		writeTestFile(t, dir, ".docker/config.json", `{
			"auths": {"ghcr.io": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("docker-user:docker-token")) + `"}}
		}`)
		_, token, cred := resolveCredentials("ghcr.io", "org/app", Config{})
		if token != "" {
			t.Errorf("token = %q, want empty", token)
		}
		if cred.Username != "docker-user" || cred.Password != "docker-token" {
			t.Errorf("resolved cred = %+v, want docker config credential", cred)
		}
	})
}

func TestDockerConfigPathsDedupe(t *testing.T) {
	clearCredEnv(t) // clears XDG_CONFIG_HOME, isolates HOME

	t.Run("no duplicates when XDG_CONFIG_HOME unset", func(t *testing.T) {
		paths := dockerConfigPaths()
		seen := map[string]bool{}
		for _, p := range paths {
			if seen[p] {
				t.Errorf("duplicate path %q", p)
			}
			seen[p] = true
		}
		// ~/.config/containers/auth.json and ~/.docker/config.json must both
		// still be present exactly once.
		home, _ := os.UserHomeDir()
		for _, want := range []string{
			filepath.Join(home, ".config", "containers", "auth.json"),
			filepath.Join(home, ".docker", "config.json"),
		} {
			if !seen[want] {
				t.Errorf("missing expected path %q in %v", want, paths)
			}
		}
	})

	t.Run("order preserved with XDG_CONFIG_HOME set", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		paths := dockerConfigPaths()
		want := filepath.Join(xdg, "containers", "auth.json")
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
			if found && p != want {
				break
			}
		}
		if !found {
			t.Errorf("XDG path %q missing from %v", want, paths)
		}
	})
}
