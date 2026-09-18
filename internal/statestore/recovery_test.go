package statestore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwss "github.com/hashicorp/terraform-plugin-framework/statestore"

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

// Cancel exactly when the version-tag PUT arrives, after state publication.
// A cancelled history step must not erase the known partial outcome.
func TestStateStoreCancelledVersionStillReportsPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := newFakeOCIRegistry()
	versionAttempt := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+testVersionTagPrefix+"1") {
			once.Do(func() { close(versionAttempt) })
			cancel()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	defer server.Close()
	store, _ := newConfiguredStore(t, "", server.Listener.Addr().String(), 2)
	written := &fwss.WriteResponse{}
	store.Write(ctx, fwss.WriteRequest{StateID: "default", StateBytes: []byte("synthetic-recovery-state")}, written)
	select {
	case <-versionAttempt:
	default:
		t.Fatal("version publication was not reached")
	}
	if len(written.Diagnostics) != 1 || written.Diagnostics[0].Summary() != "State version publication failed" {
		t.Fatalf("cancelled version misclassified: %v", written.Diagnostics)
	}
	read := &fwss.ReadResponse{}
	store.Read(context.Background(), fwss.ReadRequest{StateID: "default"}, read)
	if read.Diagnostics.HasError() || string(read.StateBytes) != "synthetic-recovery-state" {
		t.Fatal("confirmed state publication was not readable")
	}
	if reg.HasTag(testVersionTagPrefix + "1") {
		t.Fatal("cancelled version was unexpectedly published")
	}
}
