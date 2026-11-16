package download

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/afero"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
	"k8s.io/apimachinery/pkg/util/wait"
)

var defaultBackoff = wait.Backoff{
	Duration: 2 * time.Second,
	Factor:   2,
	Cap:      30 * time.Second,
	Steps:    6,
}

func Download(ctx context.Context, client *http.Client, fs afero.Fs, url, expectedDigest, destDir string) error {
	return DownloadWithRetries(ctx, client, fs, url, expectedDigest, destDir, defaultBackoff)
}

func DownloadWithRetries(ctx context.Context, client *http.Client, fs afero.Fs, url, expectedDigest, destDir string, backoff wait.Backoff) error {
	var data []byte
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		var fetchErr error
		data, fetchErr = fetchBundle(ctx, client, url)
		if fetchErr != nil {
			lastErr = fetchErr
			slog.Warn("bundle download failed, retrying", "error", fetchErr)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return errors.Join(lastErr, err)
	}

	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if digest != expectedDigest {
		return fmt.Errorf("digest mismatch: got %s, expected %s", digest, expectedDigest)
	}
	return extract(fs, data, destDir)
}

func fetchBundle(ctx context.Context, client *http.Client, url string) (_ []byte, retErr error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, resp.Body.Close()) }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(collection.MaxBundleSize)+1))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if len(data) > collection.MaxBundleSize {
		return nil, fmt.Errorf("bundle exceeds maximum size %d", collection.MaxBundleSize)
	}
	return data, nil
}

func extract(fs afero.Fs, data []byte, destDir string) (err error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("opening gzip: %w", err)
	}
	defer func() {
		closeErr := gr.Close()
		err = errors.Join(err, closeErr)
	}()

	tr := tar.NewReader(gr)
	var hdr *tar.Header
	for {
		hdr, err = tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		var dest string
		dest, err = safePath(destDir, hdr.Name)
		if err != nil {
			return err
		}

		if err := fs.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", hdr.Name, err)
		}

		var content []byte
		content, err = io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("reading %s: %w", hdr.Name, err)
		}

		if err := afero.WriteFile(fs, dest, content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", hdr.Name, err)
		}
	}

	return nil
}

func safePath(root, name string) (string, error) {
	clean := filepath.Clean(strings.TrimPrefix(name, "./"))
	if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
		return "", fmt.Errorf("unsafe tar entry: %s", name)
	}
	dest := filepath.Join(root, clean)
	if !strings.HasPrefix(dest, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root: %s", name)
	}
	return dest, nil
}
