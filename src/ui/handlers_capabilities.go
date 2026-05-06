package ui

import (
	"fmt"
	"net/http"

	authorizationv1 "k8s.io/api/authorization/v1"
)

// capabilitiesResponse describes what the cluster will actually let the
// worker do at run time. Surfaces the gap between "the source has Phase-2
// configured" and "the cluster RBAC permits Phase-2", which is otherwise
// invisible until a backup run lands a `pods is forbidden` error in the
// Job log.
type capabilitiesResponse struct {
	WorkerServiceAccount string `json:"workerServiceAccount"`
	Namespace            string `json:"namespace"`
	// Phase2Allowed is true iff a SubjectAccessReview against the worker SA
	// returns Allowed=true for `pods: create` in Namespace. The chart's
	// restoreVerification.enableEphemeralPodSpawn=true is the supported way
	// to flip this on; manual ClusterRoleBinding edits also work.
	Phase2Allowed bool   `json:"phase2Allowed"`
	Reason        string `json:"reason"`
}

// handleAPIClusterCapabilities answers "would Phase-2 restore-verification
// actually work for this operator install?" by running a SubjectAccessReview
// against the worker SA's pods/create permission. The UI reads this once
// per session and renders a warning when a user picks schema-only / sample
// / full while Phase2Allowed is false.
//
// We deliberately return 200 with Phase2Allowed=false on RBAC-check
// failures rather than 5xx — the answer "we don't know, treat as denied"
// is more useful to the UI than an opaque error, and the Reason field
// carries the diagnostic. A genuine 5xx would just hide the same
// information behind a generic "operation failed" surface.
func (s *Server) handleAPIClusterCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiResponse{Message: "GET required"})
		return
	}

	resp := capabilitiesResponse{
		WorkerServiceAccount: s.cfg.WorkerServiceAccount,
		Namespace:            s.cfg.Namespace,
	}

	if s.cfg.WorkerServiceAccount == "" {
		// Without the SA name, the SAR can't be scoped meaningfully.
		// Treat as unknown rather than guessing — the UI will show the
		// reason verbatim so an operator can see why the check is
		// inconclusive.
		resp.Reason = "WORKER_SERVICE_ACCOUNT not configured on the operator; capability check skipped"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: fmt.Sprintf("system:serviceaccount:%s:%s", s.cfg.Namespace, s.cfg.WorkerServiceAccount),
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: s.cfg.Namespace,
				Verb:      "create",
				Resource:  "pods",
			},
		},
	}
	// SubjectAccessReview is a virtual resource — Create does not persist
	// anything; the apiserver computes the answer and returns it in
	// sar.Status. Documented K8s authz pattern.
	if err := s.cfg.Client.Create(r.Context(), sar); err != nil {
		s.cfg.Logger.Error(err, "subjectaccessreview against worker SA failed",
			"sa", s.cfg.WorkerServiceAccount, "namespace", s.cfg.Namespace)
		resp.Reason = "RBAC check failed: " + sanitizeError(err) +
			" (operator SA likely missing authorization.k8s.io/subjectaccessreviews:create)"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.Phase2Allowed = sar.Status.Allowed
	switch {
	case sar.Status.Allowed:
		resp.Reason = fmt.Sprintf("worker SA %q can create pods in %q",
			s.cfg.WorkerServiceAccount, s.cfg.Namespace)
	case sar.Status.Reason != "":
		// Apiserver provided a specific reason (e.g. "no RBAC policy matched").
		resp.Reason = sar.Status.Reason
	default:
		resp.Reason = "worker SA cannot create pods. Set restoreVerification.enableEphemeralPodSpawn=true in the chart values, or pick stream-validate (no RBAC required)."
	}
	writeJSON(w, http.StatusOK, resp)
}
