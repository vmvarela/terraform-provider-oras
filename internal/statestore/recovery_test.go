package statestore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/vmvarela/terraform-provider-oras/internal/oras"
)

func TestPublicationRecoveryDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, summary, evidence string
		err                     error
	}{
		{"cancelled", "State write interrupted", "Publication is uncertain", context.Canceled},
		{"deadline", "State write interrupted", "Publication is uncertain", context.DeadlineExceeded},
		{"ambiguous", "State publication conflict", "Publication is uncertain", oras.ErrMutableTagMoved},
		{"other failure", "Failed to write state", "do not assume nothing was written", errors.New("connection lost")},
		{"partial", "State version publication failed", "published successfully", &oras.PartialPublicationError{StateTag: "state-tag", VersionTag: "version-tag", Err: oras.ErrMutableTagMoved}},
		{"cancelled version after state published", "State version publication failed", "published successfully", fmt.Errorf("wrapped: %w", &oras.PartialPublicationError{StateTag: "state-tag", VersionTag: "version-tag", Err: context.Canceled})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics diag.Diagnostics
			addPublicationDiagnostic(&diagnostics, "default", tc.err)
			if len(diagnostics) != 1 || diagnostics[0].Summary() != tc.summary {
				t.Fatalf("unexpected publication diagnostic: %v", diagnostics)
			}
			detail := diagnostics[0].Detail()
			for _, required := range []string{tc.evidence, "Do not blindly re-apply", "Stop other writers", "errored.tfstate", "lineage, serial", "fresh lock"} {
				if !strings.Contains(detail, required) {
					t.Errorf("missing recovery guidance %q", required)
				}
			}
			if strings.Contains(detail, "Re-run the operation") {
				t.Error("unsafe re-apply advice")
			}
		})
	}
}
