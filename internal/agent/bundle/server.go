package bundle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type Server struct {
	store  *BundleStore
	server *http.Server
	log    *slog.Logger
	ready  atomic.Bool
}

func NewServer(store *BundleStore, addr string, log *slog.Logger) *Server {
	s := &Server{
		store: store,
		log:   log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents/{name}/bundle", s.serveBundle)

	s.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
}

func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.server.Addr, err)
	}

	s.ready.Store(true)
	s.log.Info("bundle server listening", "addr", lis.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		if err := s.server.Serve(lis); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(shutdownCtx)
}

func (s *Server) Name() string { return "bundle-server" }

func (s *Server) Ready(_ *http.Request) error {
	if !s.ready.Load() {
		return fmt.Errorf("bundle server not ready")
	}
	return nil
}

func (s *Server) serveBundle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	digest, ok := s.store.Resolve(name)
	if !ok {
		http.NotFound(w, r)
		return
	}

	reader, size, err := s.store.Open(digest)
	if err != nil {
		s.log.Error("opening bundle", "agent", name, "digest", digest, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("ETag", digest)
	if _, err := io.Copy(w, reader); err != nil {
		s.log.Warn("streaming bundle", "agent", name, "error", err)
	}
}
