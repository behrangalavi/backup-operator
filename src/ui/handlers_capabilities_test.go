package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/go-logr/logr"
)

// makeServer wires a Server with the given client + worker SA and only
// the fields handleAPIClusterCapabilities reads. Avoids dragging the
// rest of ui.New's setup (templates, SSE broker) into this test.
func makeCapabilitiesServer(c client.Client, workerSA, namespace string) *Server {
	return &Server{
		cfg: Config{
			Namespace:            namespace,
			Client:               c,
			Logger:               logr.Discard(),
			WorkerServiceAccount: workerSA,
		},
	}
}

func decodeCapabilities(t *testing.T, rr *httptest.ResponseRecorder) capabilitiesResponse {
	t.Helper()
	var resp capabilitiesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rr.Body.String())
	}
	return resp
}

func TestCapabilities_EmptyWorkerSA(t *testing.T) {
	// When WORKER_SERVICE_ACCOUNT is not configured, the endpoint cannot
	// scope the SAR meaningfully; it must report unknown rather than guess.
	srv := makeCapabilitiesServer(fake.NewClientBuilder().Build(), "", "backup")

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.handleAPIClusterCapabilities(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp := decodeCapabilities(t, rr)
	if resp.Phase2Allowed {
		t.Error("Phase2Allowed must default to false when SA is unknown")
	}
	if !strings.Contains(resp.Reason, "WORKER_SERVICE_ACCOUNT") {
		t.Errorf("reason should explain the missing config, got %q", resp.Reason)
	}
}

func TestCapabilities_SARAllowed(t *testing.T) {
	// Apiserver permits pods/create for the worker SA → endpoint reports
	// Phase2Allowed=true and includes the SA name in the human-readable
	// reason so operators can confirm WHICH SA was checked.
	base := fake.NewClientBuilder().Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			sar, ok := obj.(*authorizationv1.SubjectAccessReview)
			if !ok {
				t.Fatalf("expected SubjectAccessReview, got %T", obj)
			}
			expectUser := "system:serviceaccount:backup:backup-operator-worker"
			if sar.Spec.User != expectUser {
				t.Errorf("SAR User = %q, want %q", sar.Spec.User, expectUser)
			}
			ra := sar.Spec.ResourceAttributes
			if ra == nil || ra.Verb != "create" || ra.Resource != "pods" || ra.Namespace != "backup" {
				t.Errorf("SAR ResourceAttributes wrong: %#v", ra)
			}
			sar.Status.Allowed = true
			return nil
		},
	})
	srv := makeCapabilitiesServer(c, "backup-operator-worker", "backup")

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.handleAPIClusterCapabilities(rr, req)

	resp := decodeCapabilities(t, rr)
	if !resp.Phase2Allowed {
		t.Error("Phase2Allowed should be true when SAR returns Allowed")
	}
	if !strings.Contains(resp.Reason, "backup-operator-worker") {
		t.Errorf("reason should name the SA, got %q", resp.Reason)
	}
}

func TestCapabilities_SARDenied(t *testing.T) {
	// Apiserver denies pods/create → endpoint reports Phase2Allowed=false
	// and surfaces a help string pointing at the correct chart toggle.
	base := fake.NewClientBuilder().Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			sar := obj.(*authorizationv1.SubjectAccessReview)
			sar.Status.Allowed = false
			// Apiserver-supplied reasons are surfaced verbatim when present.
			sar.Status.Reason = ""
			return nil
		},
	})
	srv := makeCapabilitiesServer(c, "backup-operator-worker", "backup")

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.handleAPIClusterCapabilities(rr, req)

	resp := decodeCapabilities(t, rr)
	if resp.Phase2Allowed {
		t.Error("Phase2Allowed must be false when SAR denies")
	}
	if !strings.Contains(resp.Reason, "enableEphemeralPodSpawn") {
		t.Errorf("denied reason should point to the chart toggle, got %q", resp.Reason)
	}
}

func TestCapabilities_SARCreateError(t *testing.T) {
	// SAR Create itself fails (e.g. operator SA missing the
	// authorization.k8s.io permission). Endpoint must NOT 5xx — that
	// would hide the diagnostic from the UI. Phase2Allowed falls to
	// false and the reason carries the underlying error.
	base := fake.NewClientBuilder().Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
			return errors.New("forbidden: cannot create subjectaccessreviews")
		},
	})
	srv := makeCapabilitiesServer(c, "backup-operator-worker", "backup")

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.handleAPIClusterCapabilities(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("endpoint must return 200 on RBAC-check failure, got %d", rr.Code)
	}
	resp := decodeCapabilities(t, rr)
	if resp.Phase2Allowed {
		t.Error("Phase2Allowed must be false when the check itself fails")
	}
	if !strings.Contains(resp.Reason, "subjectaccessreviews") {
		t.Errorf("reason should hint at the missing permission, got %q", resp.Reason)
	}
}

func TestCapabilities_NonGETRejected(t *testing.T) {
	srv := makeCapabilitiesServer(fake.NewClientBuilder().Build(), "x", "backup")
	req := httptest.NewRequest(http.MethodPost, "/api/cluster/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.handleAPIClusterCapabilities(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}
