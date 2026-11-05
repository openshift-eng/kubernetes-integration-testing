package naming

import (
	"crypto/sha256"
	"fmt"
	"regexp"
)

// MaxClusterNameLength is the Kubernetes DNS label limit (RFC 1123).
// TODO: remove once KitCluster becomes a singleton named "cluster" and the
// name is no longer used as metadata.name.
const MaxClusterNameLength = 63

var dnsLabelRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ContainerName returns a deterministic, length-safe container name for a
// cluster. The hash keeps the identifier well within DNS label (63 char) and
// X.509 CN (64 char) limits regardless of the human cluster name length.
func ContainerName(clusterName string) string {
	h := sha256.Sum256([]byte(clusterName))
	return fmt.Sprintf("kit-%x", h[:6])
}

// ValidateClusterName enforces RFC 1123 DNS label rules on cluster names.
// This prevents path-traversal attacks (the name is used in filesystem paths)
// and ensures the name is safe as a Kubernetes resource name.
func ValidateClusterName(name string) error {
	if len(name) == 0 || len(name) > MaxClusterNameLength {
		return fmt.Errorf("invalid cluster name: must be 1-%d characters", MaxClusterNameLength)
	}
	if !dnsLabelRegexp.MatchString(name) {
		return fmt.Errorf("invalid cluster name %q: must be a valid DNS label (lowercase alphanumeric and hyphens)", name)
	}
	return nil
}
