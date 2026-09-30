package image

import (
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func (r *Registry) ImageConfig(ref string) (*v1.Config, error) {
	img, err := r.pull(ref)
	if err != nil {
		return nil, err
	}
	cfgFile, err := img.raw.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading config for %q: %w", ref, err)
	}
	return &cfgFile.Config, nil
}
