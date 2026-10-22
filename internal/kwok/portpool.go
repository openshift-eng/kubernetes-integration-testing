package kwok

import (
	"fmt"
	"net"
	"sync"
)

// portPool serialises port allocation to avoid collisions when creating
// multiple kwokctl clusters in parallel.
type portPool struct {
	mu   sync.Mutex
	used map[int]struct{}
}

func newPortPool() *portPool {
	return &portPool{used: make(map[int]struct{})}
}

func (p *portPool) Get() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for range 10 {
		port, err := getFreePort()
		if err != nil {
			return 0, err
		}
		if _, taken := p.used[port]; taken {
			continue
		}
		p.used[port] = struct{}{}
		return port, nil
	}
	return 0, fmt.Errorf("could not find a free port after 10 attempts")
}

func (p *portPool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}

func getFreePort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("allocating free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("closing listener for port %d: %w", port, err)
	}
	return port, nil
}
