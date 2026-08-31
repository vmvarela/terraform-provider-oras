// Package oras provides OCI registry operations for the Terraform state backend.
package oras

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	orasRegistry "oras.land/oras-go/v2/registry"
	orasAuth "oras.land/oras-go/v2/registry/remote/auth"
)

// cliCredentialBlock is one oci_credentials block from the Terraform CLI config.
type cliCredentialBlock struct {
	ConfigKey               string   `hcl:"config_key,label"`
	Username                string   `hcl:"username,optional"`
	Password                string   `hcl:"password,optional"`
	AccessToken             string   `hcl:"access_token,optional"`
	RefreshToken            string   `hcl:"refresh_token,optional"`
	DockerCredentialsHelper string   `hcl:"docker_credentials_helper,optional"`
	Remain                  hcl.Body `hcl:",remain"`
}

// cliDefaultCredentialsBlock is the single oci_default_credentials block.
type cliDefaultCredentialsBlock struct {
	DockerCredentialsHelper string   `hcl:"docker_credentials_helper,optional"`
	Remain                  hcl.Body `hcl:",remain"`
}

// cliConfig is the parsed Terraform CLI config relevant to OCI credentials.
type cliConfig struct {
	Credentials     []cliCredentialBlock      `hcl:"oci_credentials,block"`
	DefaultProvider *cliDefaultCredentialsBlock `hcl:"oci_default_credentials,block"`
}

// parseConfigKey splits a config key ("ghcr.io", "ghcr.io/org", "") into
// domain and path. Bare domains have an empty path. Keys with a tag or digest
// are rejected.
func parseConfigKey(key string) (domain, path string, ok bool) {
	if key == "" {
		return "", "", true
	}
	if !hasSlash(key) {
		return key, "", true
	}
	ref, err := orasRegistry.ParseReference(key)
	if err != nil {
		return "", "", false
	}
	if ref.Reference != "" {
		// Key contains a tag or digest — not a valid config key.
		return "", "", false
	}
	return ref.Registry, ref.Repository, true
}

func hasSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

// configKeyMatch returns the specificity of a config key for the given
// registry domain and repository path. 0 = no match; 1 = global (empty key);
// 2 = domain-only; 2+N = domain + N matching path segments. Domain must match
// exactly; the key path must be a segment-wise prefix of the want path.
func configKeyMatch(key, domain, path string) int {
	if key == "" {
		return 1
	}
	kd, kp, ok := parseConfigKey(key)
	if !ok || kd != domain {
		return 0
	}
	if kp == "" {
		return 2
	}
	keySegs := splitPath(kp)
	wantSegs := splitPath(path)
	if len(keySegs) > len(wantSegs) {
		return 0
	}
	for i, seg := range keySegs {
		if seg != wantSegs[i] {
			return 0
		}
	}
	return 2 + len(keySegs)
}

func splitPath(p string) []string {
	var segs []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				segs = append(segs, p[start:i])
			}
			start = i + 1
		}
	}
	return segs
}

// cliConfigPath returns the Terraform CLI config path to inspect:
// TF_CLI_CONFIG_FILE, TERRAFORM_CONFIG, or ~/.terraformrc.
func cliConfigPath() string {
	if p := os.Getenv("TF_CLI_CONFIG_FILE"); p != "" {
		return p
	}
	if p := os.Getenv("TERRAFORM_CONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".terraformrc")
	}
	return ""
}

// loadCLIConfig parses the Terraform CLI config file at path.
// Missing files return (nil, nil). Invalid blocks return an error; callers
// log the error and skip the CLI-config sources (the user explicitly
// configured them, so the skip is surfaced via slog.Warn, not silently).
func loadCLIConfig(path string) (*cliConfig, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading Terraform CLI config %s: %w", path, err)
	}
	parser := hclparse.NewParser()
	hclFile, diags := parser.ParseHCL(data, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parsing Terraform CLI config %s: %s", path, diags.Error())
	}
	cfg := &cliConfig{}
	diags = gohcl.DecodeBody(hclFile.Body, nil, cfg)
	if diags.HasErrors() {
		return nil, fmt.Errorf("decoding Terraform CLI config %s: %s", path, diags.Error())
	}
	for _, block := range cfg.Credentials {
		if err := validateCLICredentialBlock(block); err != nil {
			return nil, fmt.Errorf("invalid oci_credentials block in %s: %w", path, err)
		}
	}
	return cfg, nil
}

// validateCLICredentialBlock enforces exactly one credential group per block:
// basic (username+password), oauth (access_token), or helper.
func validateCLICredentialBlock(block cliCredentialBlock) error {
	basic := block.Username != "" || block.Password != ""
	oauth := block.AccessToken != ""
	helper := block.DockerCredentialsHelper != ""

	groups := 0
	for _, g := range []bool{basic, oauth, helper} {
		if g {
			groups++
		}
	}
	if groups == 0 {
		return fmt.Errorf("oci_credentials %q: no credentials configured (need username/password, access_token, or docker_credentials_helper)", block.ConfigKey)
	}
	if groups > 1 {
		return fmt.Errorf("oci_credentials %q: exactly one credential group allowed", block.ConfigKey)
	}
	if basic && (block.Username == "" || block.Password == "") {
		return fmt.Errorf("oci_credentials %q: username and password must both be set", block.ConfigKey)
	}
	return nil
}

// cliBlockCredential converts a validated oci_credentials block into a
// credential, invoking the docker helper if configured.
func cliBlockCredential(ctx context.Context, block cliCredentialBlock, domain string) (orasAuth.Credential, bool) {
	switch {
	case block.DockerCredentialsHelper != "":
		return dockerHelperCredential(ctx, block.DockerCredentialsHelper, domain)
	case block.AccessToken != "":
		cred := tokenCredential(domain, block.AccessToken)
		if domain != "ghcr.io" {
			// GHCR requires basic auth; a RefreshToken would push oras-go
			// into the OAuth2 refresh grant, bypassing the exchange.
			cred.RefreshToken = block.RefreshToken
		}
		return cred, true
	default:
		return orasAuth.Credential{Username: block.Username, Password: block.Password}, true
	}
}

// credCandidate pairs a credential with its specificity.
type credCandidate struct {
	cred orasAuth.Credential
	spec int
}

// configuredCredentialCandidates collects matched candidates in priority order:
// (1) oci_credentials blocks, (2) oci_default_credentials global helper,
// (3) Docker/containers config files. First-wins on equal specificity.
func configuredCredentialCandidates(ctx context.Context, domain, path string) []credCandidate {
	var candidates []credCandidate
	add := func(key string, cred orasAuth.Credential, ok bool) {
		if !ok {
			return
		}
		if spec := configKeyMatch(key, domain, path); spec > 0 {
			candidates = append(candidates, credCandidate{cred: cred, spec: spec})
		}
	}

	if cliCfg, err := loadCLIConfig(cliConfigPath()); err != nil {
		// Warn: the user explicitly configured credentials, so a rejected
		// .terraformrc block must not fail silently.
		slog.Warn("failed to load Terraform CLI config for OCI credentials; skipping cli-configured credentials", "error", err)
	} else if cliCfg != nil {
		for _, block := range cliCfg.Credentials {
			cred, ok := cliBlockCredential(ctx, block, domain)
			add(block.ConfigKey, cred, ok)
		}
		if cliCfg.DefaultProvider != nil && cliCfg.DefaultProvider.DockerCredentialsHelper != "" {
			cred, ok := dockerHelperCredential(ctx, cliCfg.DefaultProvider.DockerCredentialsHelper, domain)
			add("", cred, ok)
		}
	}

	for _, cfgPath := range dockerConfigPaths() {
		if _, err := os.Stat(cfgPath); err != nil {
			continue
		}
		cfg, err := loadDockerConfig(cfgPath)
		if err != nil {
			slog.Debug("skipping unreadable Docker config file", "path", cfgPath, "error", err)
			continue
		}
		for key, entry := range cfg.Auths {
			cred, ok := decodeDockerAuth(entry.Auth)
			add(key, cred, ok)
		}
		for key, helper := range cfg.CredHelpers {
			cred, ok := dockerHelperCredential(ctx, helper, domain)
			add(key, cred, ok)
		}
		if cfg.CredsStore != "" {
			cred, ok := dockerHelperCredential(ctx, cfg.CredsStore, domain)
			add("", cred, ok)
		}
	}
	return candidates
}

// resolveConfiguredCredential picks the highest-specificity configured
// credential for the registry. First candidate wins on ties. Returns false
// when nothing matches, so the caller falls through to anonymous access.
func resolveConfiguredCredential(ctx context.Context, registryDomain, repositoryPath string) (orasAuth.Credential, bool) {
	best := credCandidate{spec: 0}
	for _, cand := range configuredCredentialCandidates(ctx, registryDomain, repositoryPath) {
		if cand.spec > best.spec {
			best = cand
		}
	}
	if best.spec == 0 {
		return orasAuth.Credential{}, false
	}
	return best.cred, true
}
