package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
)

// shrinkServerVersionRetry makes the retry loop run in milliseconds.
func shrinkServerVersionRetry(t *testing.T) {
	t.Helper()
	origInterval, origTimeout := serverVersionRetryInterval, serverVersionRetryTimeout
	serverVersionRetryInterval, serverVersionRetryTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		serverVersionRetryInterval, serverVersionRetryTimeout = origInterval, origTimeout
	})
}

type versionDiscovery struct {
	discovery.DiscoveryInterface
	serverVersion func() (*version.Info, error)
}

func (f *versionDiscovery) ServerVersion() (*version.Info, error) {
	return f.serverVersion()
}

func TestServerKubeVersion(t *testing.T) {
	for _, tc := range []struct {
		gitVersion string
		want       string
	}{
		{"v1.36.4", "v1.36.4"},
		{"v1.36.4+e9d3c1b", "v1.36.4"},
	} {
		disc := &versionDiscovery{serverVersion: func() (*version.Info, error) {
			return &version.Info{GitVersion: tc.gitVersion}, nil
		}}

		got, err := ServerKubeVersion(context.Background(), &Clients{Discovery: disc})
		require.NoError(t, err, tc.gitVersion)
		assert.Equal(t, tc.want, got, tc.gitVersion)
	}
}

func TestServerKubeVersionRetriesTransientErrors(t *testing.T) {
	shrinkServerVersionRetry(t)

	calls := 0
	disc := &versionDiscovery{serverVersion: func() (*version.Info, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("connection refused")
		}
		return &version.Info{GitVersion: "v1.36.4"}, nil
	}}

	got, err := ServerKubeVersion(context.Background(), &Clients{Discovery: disc})
	require.NoError(t, err)
	assert.Equal(t, "v1.36.4", got)
	assert.Equal(t, 3, calls)
}

func TestServerKubeVersionFailsAfterRetries(t *testing.T) {
	shrinkServerVersionRetry(t)

	disc := &versionDiscovery{serverVersion: func() (*version.Info, error) {
		return nil, errors.New("connection refused")
	}}

	_, err := ServerKubeVersion(context.Background(), &Clients{Discovery: disc})
	require.Error(t, err)
	assert.ErrorContains(t, err, "getting API server version")
	assert.ErrorContains(t, err, "connection refused")
}

func TestServerKubeVersionFailsFastOnBadVersion(t *testing.T) {
	shrinkServerVersionRetry(t)

	for _, bad := range []string{"garbage", "v1.36"} {
		calls := 0
		disc := &versionDiscovery{serverVersion: func() (*version.Info, error) {
			calls++
			return &version.Info{GitVersion: bad}, nil
		}}

		_, err := ServerKubeVersion(context.Background(), &Clients{Discovery: disc})
		require.Error(t, err, bad)
		assert.ErrorContains(t, err, "unexpected API server version", bad)
		assert.Equal(t, 1, calls, "%s: a version that cannot be canonicalized must not be retried", bad)
	}
}
