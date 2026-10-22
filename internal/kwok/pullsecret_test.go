package kwok

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetData(t *testing.T) {
	var p PullSecretProvider
	input := []byte("secret")
	p.Set(input)

	assert.Equal(t, []byte("secret"), p.Data())

	input[0] = 'X'
	assert.Equal(t, []byte("secret"), p.Data(), "Set should copy the input, not alias it")

	got := p.Data()
	got[0] = 'Y'
	assert.Equal(t, []byte("secret"), p.Data(), "Data should return a copy, not the internal slice")
}

func TestDataReturnsNilWhenEmpty(t *testing.T) {
	var p PullSecretProvider
	assert.Nil(t, p.Data())
}

func TestSetFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pull-secret.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"auths":{}}`), 0o600))

	var p PullSecretProvider
	require.NoError(t, p.SetFromFile(path))
	assert.Equal(t, []byte(`{"auths":{}}`), p.Data())
}

func TestSetFromFileMissing(t *testing.T) {
	var p PullSecretProvider
	assert.Error(t, p.SetFromFile("/nonexistent"))
}

func TestWithAuthFileNoData(t *testing.T) {
	var p PullSecretProvider
	var called bool
	err := p.WithAuthFile(func(path string) error {
		called = true
		assert.Empty(t, path, "path should be empty when no data is set")
		return nil
	})
	require.NoError(t, err)
	assert.True(t, called, "callback was not called")
}

func TestWithAuthFileWritesTemp(t *testing.T) {
	var p PullSecretProvider
	p.Set([]byte(`{"auths":{"reg":{"auth":"dG9rZW4="}}}`))

	var tempPath string
	err := p.WithAuthFile(func(path string) error {
		tempPath = path
		require.NotEmpty(t, path)

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, `{"auths":{"reg":{"auth":"dG9rZW4="}}}`, string(data))

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		return nil
	})
	require.NoError(t, err)

	_, err = os.Stat(tempPath)
	assert.True(t, os.IsNotExist(err), "temp file should be removed after WithAuthFile returns")
}

func TestWithAuthFilePropagatesError(t *testing.T) {
	var p PullSecretProvider
	p.Set([]byte("data"))

	var tempPath string
	want := errors.New("callback failed")
	err := p.WithAuthFile(func(path string) error {
		tempPath = path
		return want
	})
	assert.ErrorIs(t, err, want)

	_, statErr := os.Stat(tempPath)
	assert.True(t, os.IsNotExist(statErr), "temp file should be removed even when callback fails")
}
