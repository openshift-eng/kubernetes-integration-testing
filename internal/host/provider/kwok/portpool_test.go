package kwok

import (
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortPoolGetReturnsUsablePort(t *testing.T) {
	pp := newPortPool()

	port, err := pp.Get()
	require.NoError(t, err)

	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	require.NoError(t, err, "port %d should be bindable", port)
	require.NoError(t, l.Close())
}

func TestPortPoolReleaseAllowsReuse(t *testing.T) {
	pp := newPortPool()

	port, err := pp.Get()
	require.NoError(t, err)

	pp.Release(port)

	assert.NotContains(t, pp.used, port)
}

func TestPortPoolConcurrentGet(t *testing.T) {
	pp := newPortPool()
	n := 20

	ports := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			p, err := pp.Get()
			require.NoError(t, err)
			ports[idx] = p
		}(i)
	}
	wg.Wait()

	seen := make(map[int]struct{}, n)
	for _, p := range ports {
		assert.NotContains(t, seen, p, "duplicate port %d", p)
		seen[p] = struct{}{}
	}
}
