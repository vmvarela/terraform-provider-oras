package oras

import (
	"context"
	"fmt"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasRegistry "oras.land/oras-go/v2/registry"
)

// workspaceIDFromTag distinguishes provider tags from unrelated OCI artifacts.
// The full tag and the original-name annotation must both validate: a legacy
// literal name can look exactly like a SHA-256 identifier.
func workspaceIDFromTag(tag string) (string, bool) {
	for _, prefix := range []string{stateTagPrefix, lockTagPrefix, unlockedTagPrefix} {
		if strings.HasPrefix(tag, prefix) {
			return strings.TrimPrefix(tag, prefix), true
		}
	}
	if strings.HasPrefix(tag, stateVersionTagPrefix) {
		base, _, ok := splitStateVersionTag(tag)
		if !ok {
			return "", true
		}
		return strings.TrimPrefix(base, stateVersionTagPrefix), true
	}
	return "", false
}

// reservedWorkspaceTags lists the repository and rejects legacy and mixed
// layouts by tag name alone, even when only locks/history remain: every
// reserved tag must carry a 64-character identifier. It reads no manifests.
// It is not cached: objects can change between RPCs.
func reservedWorkspaceTags(ctx context.Context, repo *orasRepositoryClient) ([]string, error) {
	var tags []string
	err := retry(ctx, func(ctx context.Context) error {
		tags = nil
		return repo.inner.Tags(ctx, "", func(page []string) error {
			tags = append(tags, page...)
			return nil
		})
	})
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking workspace storage format: %w", err)
	}
	var reservedTags []string
	for _, tag := range tags {
		id, reserved := workspaceIDFromTag(tag)
		if !reserved {
			continue
		}
		if len(id) != 64 {
			return nil, fmt.Errorf("legacy workspace tag %q detected: stop all writers and migrate verified state to an empty repository; do not mix provider storage formats (see docs/guides/workspace-migration.md)", tag)
		}
		if err := (orasRegistry.Reference{Reference: tag}).ValidateReferenceAsTag(); err != nil {
			return nil, err
		}
		reservedTags = append(reservedTags, tag)
	}
	return reservedTags, nil
}

// repositoryWorkspaceIdentities verifies the original-name annotation of
// every reserved tag in the repository. Used by List, which must never guess
// a name; per-workspace operations use the scoped checkWorkspace instead.
func repositoryWorkspaceIdentities(ctx context.Context, repo *orasRepositoryClient) (map[string]string, error) {
	tags, err := reservedWorkspaceTags(ctx, repo)
	if err != nil {
		return nil, err
	}
	identities := make(map[string]string)
	for _, tag := range tags {
		name, err := workspaceNameFromTag(ctx, repo, tag)
		if isNotFound(err) {
			// Concurrent deletion: no object remains to inspect.
			continue
		}
		if err != nil {
			return nil, err
		}
		identities[tag] = name
	}
	return identities, nil
}

func validateWorkspaceManifest(m ocispec.Manifest, tag, workspace string) error {
	name, ok := m.Annotations[annotationWorkspace]
	if !ok || name != workspace {
		return fmt.Errorf("workspace identity mismatch at tag %q: refusing access; stop writers and restore verified metadata (see docs/guides/workspace-migration.md)", tag)
	}
	return nil
}

// checkWorkspace is a preflight, NOT CAS. It neither fences old binaries nor
// closes the existing verify-to-write window. Never cache its registry result.
//
// Scope (ADR-0002 amendment): legacy/mixed layouts are rejected by tag name
// across the whole repository, but only this workspace's mutable tags
// (state, lock, unlocked marker) have their annotations read, so the cost is
// bounded regardless of workspace count and retained history. Version
// manifests are verified on use (versioned Put, allocation fallback and
// retention; see verifiedVersions).
func (wc *workspaceClient) checkWorkspace(ctx context.Context) error {
	for _, tag := range []string{wc.stateTag, wc.lockTag, wc.unlockedTag, wc.versionTagFor(maxStateVersion)} {
		if err := (orasRegistry.Reference{Reference: tag}).ValidateReferenceAsTag(); err != nil {
			return fmt.Errorf("invalid workspace tag: %w", err)
		}
	}
	tags, err := reservedWorkspaceTags(ctx, wc.client.repoClient)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if tag != wc.stateTag && tag != wc.lockTag && tag != wc.unlockedTag {
			continue
		}
		name, err := workspaceNameFromTag(ctx, wc.client.repoClient, tag)
		if isNotFound(err) {
			// Concurrent deletion: no object remains to inspect.
			continue
		}
		if err != nil {
			return err
		}
		if name != wc.stateID {
			return fmt.Errorf("workspace identity mismatch at tag %q; stop writers and restore verified metadata (see docs/guides/workspace-migration.md)", tag)
		}
	}
	return nil
}
