package goja

import "k8s.io/apimachinery/pkg/util/sets"

var blockedModules = sets.New[string](
	"fs", "net", "http", "https",
	"child_process", "os", "path",
	"crypto", "dgram", "dns",
	"tls", "cluster", "worker_threads",
	"vm", "process",
)
