//go:build linux

package tun

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/netlink"
	"github.com/sagernet/sing/common/logger"
)

// A netlink reader that stops closes its update channel. The monitor must not
// keep selecting the closed channel, which spins a core forever.
func TestNetworkUpdateMonitorSurvivesClosedSubscription(t *testing.T) {
	m := &networkUpdateMonitor{close: make(chan struct{}), logger: logger.NOP()}
	var emitted atomic.Int64
	m.RegisterCallback(func() { emitted.Add(1) })

	route := make(chan netlink.RouteUpdate)
	subs := &netlinkSubscriptions{
		route:   subscription[netlink.RouteUpdate]{updates: route, done: make(chan struct{})},
		link:    subscription[netlink.LinkUpdate]{updates: make(chan netlink.LinkUpdate), done: make(chan struct{})},
		address: subscription[netlink.AddrUpdate]{updates: make(chan netlink.AddrUpdate), done: make(chan struct{})},
	}
	exited := make(chan struct{})
	go func() {
		m.loopUpdate(subs, 50*time.Millisecond)
		close(exited)
	}()
	close(route)

	time.Sleep(500 * time.Millisecond)
	// One emit for the loss, at most one more once the debounce expires. A
	// spinning loop emits every 50ms.
	if n := emitted.Load(); n < 1 || n > 2 {
		t.Fatalf("emitted %d updates after a closed subscription, want 1 or 2", n)
	}
	m.Close()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("loop did not exit on Close")
	}
}
