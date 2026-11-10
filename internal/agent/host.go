package agent

import "log/slog"

type host struct {
	log  *slog.Logger
	kube KubeClient
}

func newHost(log *slog.Logger, kube KubeClient) Host {
	return &host{log: log, kube: kube}
}

func (h *host) Log(msg string) {
	h.log.Info(msg, "source", "script")
}

func (h *host) Kube() KubeClient {
	return h.kube
}
