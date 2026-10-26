package daemon

import (
	"testing"

	"github.com/spf13/afero"
)

func TestPaths(t *testing.T) {
	p := NewPaths("/data")

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"Pid", p.Pid(), "/data/pid"},
		{"Socket", p.Socket(), "/data/kit.sock"},
		{"Lock", p.Lock(), "/data/kit.lock"},
		{"Log", p.Log(), "/data/daemon.log"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestWriteAndReadPid(t *testing.T) {
	fs := afero.NewMemMapFs()

	if err := WritePid(fs, "/data/pid", 12345); err != nil {
		t.Fatal(err)
	}

	pid, err := ReadPid(fs, "/data/pid")
	if err != nil {
		t.Fatal(err)
	}
	if pid != 12345 {
		t.Errorf("ReadPid() = %d, want 12345", pid)
	}
}

func TestReadPidMissing(t *testing.T) {
	fs := afero.NewMemMapFs()

	_, err := ReadPid(fs, "/data/nope")
	if err == nil {
		t.Fatal("expected error for missing pid file")
	}
}

func TestIsRunningNoFiles(t *testing.T) {
	fs := afero.NewMemMapFs()

	pid, running := IsRunning(fs, "/data/pid", "/data/sock")
	if running {
		t.Errorf("expected not running, got pid %d", pid)
	}
}

func TestCleanupRemovesFiles(t *testing.T) {
	fs := afero.NewMemMapFs()

	_ = afero.WriteFile(fs, "/data/pid", []byte("99999"), 0o644)
	_ = afero.WriteFile(fs, "/data/sock", []byte(""), 0o644)

	Cleanup(fs, "/data/pid", "/data/sock")

	if exists, _ := afero.Exists(fs, "/data/pid"); exists {
		t.Error("pid file should be removed")
	}
	if exists, _ := afero.Exists(fs, "/data/sock"); exists {
		t.Error("socket file should be removed")
	}
}

func TestIsRunningStaleProcess(t *testing.T) {
	fs := afero.NewMemMapFs()

	_ = afero.WriteFile(fs, "/data/pid", []byte("99999"), 0o644)
	_ = afero.WriteFile(fs, "/data/sock", []byte(""), 0o644)

	_, running := IsRunning(fs, "/data/pid", "/data/sock")
	if running {
		t.Error("expected not running for stale pid")
	}

	if exists, _ := afero.Exists(fs, "/data/pid"); exists {
		t.Error("stale pid file should be cleaned up")
	}
	if exists, _ := afero.Exists(fs, "/data/sock"); exists {
		t.Error("stale socket file should be cleaned up")
	}
}
