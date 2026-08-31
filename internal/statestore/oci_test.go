// Package statestore implements the OCI state store for the Terraform plugin framework.
package statestore

import (
	"strings"
	"testing"
)

func TestParseOCIURL(t *testing.T) {
	tests := []struct {
		name       string
		rawURL     string
		wantReg    string
		wantRepo   string
		wantErr    bool
		wantErrSub string // substring expected in error message
	}{
		// Valid URLs.
		{
			name:     "simple registry and repo",
			rawURL:   "oci://ghcr.io/myorg/infra-state",
			wantReg:  "ghcr.io",
			wantRepo: "myorg/infra-state",
		},
		{
			name:     "single path segment",
			rawURL:   "oci://registry.example.com/myrepo",
			wantReg:  "registry.example.com",
			wantRepo: "myrepo",
		},
		{
			name:     "registry with port",
			rawURL:   "oci://registry.example.com:5000/myrepo",
			wantReg:  "registry.example.com:5000",
			wantRepo: "myrepo",
		},
		{
			name:     "uppercase host passes through",
			rawURL:   "oci://GHCR.IO/myorg/state",
			wantReg:  "GHCR.IO",
			wantRepo: "myorg/state",
		},
		{
			name:     "deeply nested repo path",
			rawURL:   "oci://ghcr.io/org/team/app/state",
			wantReg:  "ghcr.io",
			wantRepo: "org/team/app/state",
		},
		{
			name:     "no reference tag",
			rawURL:   "oci://ghcr.io/myorg/state",
			wantReg:  "ghcr.io",
			wantRepo: "myorg/state",
		},
		// Invalid URLs.
		{
			name:       "missing oci scheme",
			rawURL:     "https://ghcr.io/myorg/state",
			wantErr:    true,
			wantErrSub: "must start with 'oci://'",
		},
		{
			name:       "empty string",
			rawURL:     "",
			wantErr:    true,
			wantErrSub: "must start with 'oci://'",
		},
		{
			name:       "scheme only",
			rawURL:     "oci://",
			wantErr:    true,
			wantErrSub: "missing the repository path",
		},
		{
			name:       "registry host only",
			rawURL:     "oci://ghcr.io",
			wantErr:    true,
			wantErrSub: "missing the repository path",
		},
		{
			name:       "empty host",
			rawURL:     "oci:///myorg/state",
			wantErr:    true,
			wantErrSub: "missing the registry host",
		},
		{
			name:       "empty repository with trailing slash",
			rawURL:     "oci://ghcr.io/",
			wantErr:    true,
			wantErrSub: "missing the repository path",
		},
		{
			name:       "path traversal with dotdot",
			rawURL:     "oci://ghcr.io/myorg/../../etc",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "dotdot inside segment",
			rawURL:     "oci://ghcr.io/myorg/secret..backup",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "leading dotdot escapes repo root",
			rawURL:     "oci://ghcr.io/../other",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "double slash mid repo",
			rawURL:     "oci://ghcr.io/myorg//state",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		// Double slash directly after the host yields repository "/state" —
		// caught by the leading-slash check, not the "//" check.
		{
			name:       "double slash after host",
			rawURL:     "oci://ghcr.io//state",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "trailing slash",
			rawURL:     "oci://ghcr.io/myorg/state/",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
		{
			name:       "double slash at repo end",
			rawURL:     "oci://ghcr.io/myorg/state//",
			wantErr:    true,
			wantErrSub: "invalid path component",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, repo, err := parseOCIURL(tt.rawURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseOCIURL(%q) = (%q, %q, nil), want error", tt.rawURL, reg, repo)
				}
				if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("parseOCIURL(%q) error = %q, want substring %q", tt.rawURL, err.Error(), tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOCIURL(%q) unexpected error: %v", tt.rawURL, err)
			}
			if reg != tt.wantReg {
				t.Errorf("parseOCIURL(%q) registry = %q, want %q", tt.rawURL, reg, tt.wantReg)
			}
			if repo != tt.wantRepo {
				t.Errorf("parseOCIURL(%q) repository = %q, want %q", tt.rawURL, repo, tt.wantRepo)
			}
		})
	}
}
