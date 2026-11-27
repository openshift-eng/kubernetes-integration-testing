package kwok

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"text/template"
)

//go:embed data
var configFS embed.FS

// writeKwokConfig writes the kwok configuration from the embedded data
// directory, joined with '---' separators: *.tmpl files are rendered
// with the cluster parameters, the rest are copied verbatim.
//
// No KwokConfiguration is provided on purpose: a partial one skips
// kwok's defaulting and leaves options like the stage parallelism at
// zero, which crash-loops the controller. The Logs CRD is enabled
// through the controller args instead.
func writeKwokConfig(path, logsDir, kubeVersion string) error {
	values := struct {
		KubeVersion string
		LogsDir     string
	}{kubeVersion, logsDir}

	entries, err := fs.ReadDir(configFS, "data")
	if err != nil {
		return fmt.Errorf("reading config data: %w", err)
	}

	var buf bytes.Buffer
	written := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := configFS.ReadFile("data/" + entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		if written {
			buf.WriteString("---\n")
		}
		if strings.HasSuffix(entry.Name(), ".tmpl") {
			tmpl, err := template.New(entry.Name()).Parse(string(content))
			if err != nil {
				return fmt.Errorf("parsing %s: %w", entry.Name(), err)
			}
			if err := tmpl.Execute(&buf, values); err != nil {
				return fmt.Errorf("rendering %s: %w", entry.Name(), err)
			}
		} else {
			buf.Write(content)
		}
		written = true
	}

	return os.WriteFile(path, buf.Bytes(), 0o644)
}
