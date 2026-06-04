package oras

import (
	"testing"
)

func TestParseGHCRRepository(t *testing.T) {
	tests := []struct {
		name        string
		repository  string
		wantHost    string
		wantOwner   string
		wantPackage string
		wantErr     bool
	}{
		{
			name:        "valid ghcr.io repository",
			repository:  "ghcr.io/myorg/myrepo",
			wantHost:    "ghcr.io",
			wantOwner:   "myorg",
			wantPackage: "myrepo",
			wantErr:     false,
		},
		{
			name:        "valid ghcr.io with nested package",
			repository:  "ghcr.io/myorg/myrepo/subpath",
			wantHost:    "ghcr.io",
			wantOwner:   "myorg",
			wantPackage: "myrepo/subpath",
			wantErr:     false,
		},
		{
			name:        "valid non-ghcr registry",
			repository:  "registry.example.com/owner/package",
			wantHost:    "registry.example.com",
			wantOwner:   "owner",
			wantPackage: "package",
			wantErr:     false,
		},
		{
			name:        "registry with port",
			repository:  "localhost:5000/owner/package",
			wantHost:    "localhost:5000",
			wantOwner:   "owner",
			wantPackage: "package",
			wantErr:     false,
		},
		{
			name:       "missing package",
			repository: "ghcr.io/myorg",
			wantErr:    true,
		},
		{
			name:       "empty string",
			repository: "",
			wantErr:    true,
		},
		{
			name:       "single segment",
			repository: "ghcr.io",
			wantErr:    true,
		},
		{
			name:       "empty host",
			repository: "/owner/package",
			wantErr:    true,
		},
		{
			name:       "empty owner",
			repository: "ghcr.io//package",
			wantErr:    true,
		},
		{
			name:       "empty package",
			repository: "ghcr.io/owner/",
			wantErr:    true,
		},
		{
			name:       "whitespace only",
			repository: "   ",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, owner, pkg, err := parseGHCRRepository(tt.repository)

			if tt.wantErr {
				if err == nil {
					t.Errorf("parseGHCRRepository(%q) expected error, got nil", tt.repository)
				}
				return
			}

			if err != nil {
				t.Errorf("parseGHCRRepository(%q) unexpected error: %v", tt.repository, err)
				return
			}

			if host != tt.wantHost {
				t.Errorf("parseGHCRRepository(%q) host = %q, want %q", tt.repository, host, tt.wantHost)
			}
			if owner != tt.wantOwner {
				t.Errorf("parseGHCRRepository(%q) owner = %q, want %q", tt.repository, owner, tt.wantOwner)
			}
			if pkg != tt.wantPackage {
				t.Errorf("parseGHCRRepository(%q) package = %q, want %q", tt.repository, pkg, tt.wantPackage)
			}
		})
	}
}

func TestTryDeleteGHCRTag_NotGHCR(t *testing.T) {
	repo := &orasRepositoryClient{
		inner:      newFakeORASRepo(),
		repository: "registry.example.com/owner/package",
	}

	err := tryDeleteGHCRTag(t.Context(), repo, "some-tag")
	if err == nil {
		t.Error("tryDeleteGHCRTag expected error for non-GHCR repository")
	}
	if err != errNotGHCR {
		t.Errorf("tryDeleteGHCRTag error = %v, want %v", err, errNotGHCR)
	}
}

func TestTryDeleteGHCRTag_NilRepo(t *testing.T) {
	err := tryDeleteGHCRTag(t.Context(), nil, "some-tag")
	if err == nil {
		t.Error("tryDeleteGHCRTag expected error for nil repository")
	}
}
