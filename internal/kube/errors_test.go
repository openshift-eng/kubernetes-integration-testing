package kube

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsTerminalError(t *testing.T) {
	gr := schema.GroupResource{Resource: "configmaps"}

	tests := []struct {
		name     string
		err      error
		terminal bool
	}{
		{"Forbidden", apierrors.NewForbidden(gr, "", nil), true},
		{"Unauthorized", apierrors.NewUnauthorized("bad token"), true},
		{"MethodNotSupported", apierrors.NewMethodNotSupported(gr, "WATCH"), true},
		{"Invalid", apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, "x", nil), true},
		{"BadRequest", apierrors.NewBadRequest("malformed selector"), true},
		{"NotFound", apierrors.NewNotFound(gr, "x"), false},
		{"ServiceUnavailable", apierrors.NewServiceUnavailable("try again"), false},
		{"ServerTimeout", apierrors.NewServerTimeout(gr, "list", 5), false},
		{"TooManyRequests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"Conflict", apierrors.NewConflict(gr, "x", nil), false},
		{"InternalError", apierrors.NewInternalError(fmt.Errorf("boom")), false},
		{"GenericError", fmt.Errorf("network blip"), false},
		{"Nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.terminal, IsTerminalError(tt.err))
		})
	}
}
