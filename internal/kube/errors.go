package kube

import apierrors "k8s.io/apimachinery/pkg/api/errors"

// IsTerminalError returns true for API errors that will never succeed on
// retry with the same request: auth failures, bad requests, and invalid
// operations. NotFound is excluded because a CRD may be installed later.
func IsTerminalError(err error) bool {
	return apierrors.IsForbidden(err) ||
		apierrors.IsUnauthorized(err) ||
		apierrors.IsMethodNotSupported(err) ||
		apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err)
}
