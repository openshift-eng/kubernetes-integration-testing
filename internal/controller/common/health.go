package common

import "net/http"

type ReadyzChecker interface {
	Name() string
	Ready(*http.Request) error
}
