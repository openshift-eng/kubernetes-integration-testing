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
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/afero"

	"github.com/openshift-eng/kubernetes-integration-testing/internal/collection"
)

func Download(ctx context.Context, client *http.Client, fs afero.Fs, url, expectedDigest, destDir string) (err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching bundle: %w", err)
	}
	defer func() {
		closeErr := resp.Body.Close()
		err = errors.Join(err, closeErr)
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching bundle: %s", resp.Status)
	}

	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, int64(collection.MaxBundleSize)+1)
	data, err := io.ReadAll(io.TeeReader(limited, hasher))
	if err != nil {
		return fmt.Errorf("reading bundle: %w", err)
	}
	if len(data) > collection.MaxBundleSize {
		return fmt.Errorf("bundle size exceeds maximum %d", collection.MaxBundleSize)
	}

	digest := fmt.Sprintf("%x", hasher.Sum(nil))
	if digest != expectedDigest {
		return fmt.Errorf("digest mismatch: got %s, expected %s", digest, expectedDigest)
	}

	return extract(fs, data, destDir)
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
