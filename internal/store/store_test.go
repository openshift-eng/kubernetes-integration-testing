package store

import (
	"fmt"
	"testing"

	"github.com/spf13/afero"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewWithRoot(afero.NewMemMapFs(), "/data")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPaths(t *testing.T) {
	s := newTestStore(t)

	if got := s.PidPath(); got != "/data/pid" {
		t.Errorf("PidPath() = %q, want /data/pid", got)
	}
	if got := s.SocketPath(); got != "/data/kit.sock" {
		t.Errorf("SocketPath() = %q, want /data/kit.sock", got)
	}
}

func TestGetFetches(t *testing.T) {
	s := newTestStore(t)

	fetched := false
	s.Register("kwokctl-0.7.0", func(dest string) error {
		fetched = true
		return afero.WriteFile(s.Fs(), dest, []byte("fake"), 0o755)
	})

	path, err := s.Get("kwokctl-0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if !fetched {
		t.Error("expected fetch to be called")
	}
	if path != "/data/cache/kwokctl-0.7.0" {
		t.Errorf("Get() = %q, want /data/cache/kwokctl-0.7.0", path)
	}
}

func TestGetCached(t *testing.T) {
	fs := afero.NewMemMapFs()
	s, _ := NewWithRoot(fs, "/data")
	if err := afero.WriteFile(fs, "/data/cache/kwokctl-0.7.0", []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}

	fetched := false
	s.Register("kwokctl-0.7.0", func(dest string) error {
		fetched = true
		return nil
	})

	path, err := s.Get("kwokctl-0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if fetched {
		t.Error("should not fetch when content is cached")
	}
	if path != "/data/cache/kwokctl-0.7.0" {
		t.Errorf("Get() = %q, want /data/cache/kwokctl-0.7.0", path)
	}
}

func TestGetUnregistered(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get("unknown-thing")
	if err == nil {
		t.Fatal("expected error for unregistered content")
	}
}

func TestGetFetchError(t *testing.T) {
	s := newTestStore(t)
	s.Register("broken", func(dest string) error {
		return fmt.Errorf("network down")
	})

	_, err := s.Get("broken")
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}
}

func TestDirsCreated(t *testing.T) {
	fs := afero.NewMemMapFs()
	_, err := NewWithRoot(fs, "/data")
	if err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"/data", "/data/cache", "/data/clusters"} {
		exists, _ := afero.DirExists(fs, dir)
		if !exists {
			t.Errorf("directory %q should exist", dir)
		}
	}
}

func TestWriteAndReadPid(t *testing.T) {
	fs := afero.NewMemMapFs()
	path := "/data/pid"

	if err := WritePid(fs, path, 12345); err != nil {
		t.Fatal(err)
	}

	pid, err := ReadPid(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 12345 {
		t.Errorf("ReadPid() = %d, want 12345", pid)
	}
}
