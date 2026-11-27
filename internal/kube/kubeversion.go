package kube

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version is a Kubernetes version, vMAJOR.MINOR[.PATCH]. Patch is -1
// when unspecified.
type Version struct {
	Major int
	Minor int
	Patch int
}

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\.(0|[1-9][0-9]*))?$`)

// ParseVersion parses vMAJOR.MINOR[.PATCH]; the "v" prefix is optional
// and the patch may be omitted.
func ParseVersion(version string) (Version, error) {
	m := versionPattern.FindStringSubmatch(version)
	if m == nil {
		return Version{}, fmt.Errorf("bad kube version %q", version)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return Version{}, err
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, err
	}
	patch := -1
	if m[3] != "" {
		if patch, err = strconv.Atoi(m[3]); err != nil {
			return Version{}, err
		}
	}
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}

// ValidateVersion reports whether version is a well-formed Kubernetes
// version.
func ValidateVersion(version string) bool {
	_, err := ParseVersion(version)
	return err == nil
}

// IsExact reports whether the patch is specified.
func (v Version) IsExact() bool {
	return v.Patch >= 0
}

func (v Version) String() string {
	if v.Patch < 0 {
		return fmt.Sprintf("v%d.%d", v.Major, v.Minor)
	}
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Matches reports whether other agrees with every component v specifies.
func (v Version) Matches(other Version) bool {
	return v.Major == other.Major && v.Minor == other.Minor &&
		(v.Patch < 0 || v.Patch == other.Patch)
}
