package naming

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerName(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		assert.Equal(t, ContainerName("my-cluster"), ContainerName("my-cluster"))
	})

	t.Run("different names produce different IDs", func(t *testing.T) {
		assert.NotEqual(t, ContainerName("cluster-a"), ContainerName("cluster-b"))
	})

	t.Run("is a valid DNS label", func(t *testing.T) {
		name := ContainerName("a-very-long-cluster-name-that-would-have-exceeded-the-old-limit-by-a-lot")
		assert.LessOrEqual(t, len(name), 63)
		assert.Regexp(t, dnsLabelRegexp, name)
	})

	t.Run("has kit prefix", func(t *testing.T) {
		assert.True(t, strings.HasPrefix(ContainerName("test"), "kit-"))
	})
}

func TestValidateClusterName(t *testing.T) {
	t.Run("valid names", func(t *testing.T) {
		for _, name := range []string{"a", "my-cluster", "cluster-123", strings.Repeat("a", MaxClusterNameLength)} {
			require.NoError(t, ValidateClusterName(name), name)
		}
	})

	t.Run("max length name fits kwok worst case", func(t *testing.T) {
		name := strings.Repeat("a", MaxClusterNameLength)
		require.NoError(t, ValidateClusterName(name))
		worstCase := "kwok-kit-" + name + "-kube-controller-manager"
		assert.LessOrEqual(t, len(worstCase), 63, "kwok container name %q exceeds DNS label limit (len=%d)", worstCase, len(worstCase))
	})

	t.Run("invalid names", func(t *testing.T) {
		cases := []struct {
			name string
			desc string
		}{
			{"My-Cluster", "uppercase"},
			{"has_underscore", "underscore"},
			{"has.dot", "dot"},
			{"-leading-hyphen", "leading hyphen"},
			{"trailing-hyphen-", "trailing hyphen"},
			{strings.Repeat("a", MaxClusterNameLength+1), "too long"},
			{"", "empty"},
			{"kit-foo", "kit prefix"},
		}
		for _, tc := range cases {
			t.Run(tc.desc, func(t *testing.T) {
				assert.Error(t, ValidateClusterName(tc.name))
			})
		}
	})
}
