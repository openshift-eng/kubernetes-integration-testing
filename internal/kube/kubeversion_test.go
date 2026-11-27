package kube

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Version
	}{
		{"1.36.4", Version{Major: 1, Minor: 36, Patch: 4}},
		{"v1.36.4", Version{Major: 1, Minor: 36, Patch: 4}},
		{"v1.36", Version{Major: 1, Minor: 36, Patch: -1}},
	} {
		got, err := ParseVersion(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}

	for _, bad := range []string{"", "v1", "1.x", "v1.36.4.1", "1.36.4-rc.1", "kindest/node:v1.36.4", "01.36.4", "1.36.04", "v01.36"} {
		_, err := ParseVersion(bad)
		assert.Error(t, err, bad)
		assert.False(t, ValidateVersion(bad), bad)
	}
}

func TestVersionMatches(t *testing.T) {
	minor := Version{Major: 1, Minor: 36, Patch: -1}
	assert.True(t, minor.Matches(Version{Major: 1, Minor: 36, Patch: 0}))
	assert.True(t, minor.Matches(Version{Major: 1, Minor: 36, Patch: 4}))
	assert.False(t, minor.Matches(Version{Major: 1, Minor: 37, Patch: 0}), "v1.36 must not match v1.37")

	exact := Version{Major: 1, Minor: 36, Patch: 4}
	assert.True(t, exact.Matches(Version{Major: 1, Minor: 36, Patch: 4}))
	assert.False(t, exact.Matches(Version{Major: 1, Minor: 36, Patch: 5}))
}
