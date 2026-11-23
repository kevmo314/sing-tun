package tun

import (
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/sagernet/netlink"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/x/list"

	"golang.org/x/sys/unix"
)

type networkUpdateMonitor struct {
	close chan struct{}

	access    sync.Mutex
	callbacks list.List[NetworkUpdateCallback]
	logger    logger.Logger
}

var ErrNetlinkBanned = E.New(
	"netlink socket in Android is banned by Google, " +
		"use the root or system (ADB) user to run sing-box, " +
		"or switch to the sing-box Android graphical interface client",
)

func NewNetworkUpdateMonitor(logger logger.Logger) (NetworkUpdateMonitor, error) {
	monitor := &networkUpdateMonitor{
		close:  make(chan struct{}),
		logger: logger,
	}
	// check is netlink banned by google
	if runtime.GOOS == "android" {
		netlinkSocket, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM, unix.NETLINK_ROUTE)
		if err != nil {
			return nil, ErrNetlinkBanned
		}
		err = unix.Bind(netlinkSocket, &unix.SockaddrNetlink{
			Family: unix.AF_NETLINK,
		})
		unix.Close(netlinkSocket)
		if err != nil {
			return nil, ErrNetlinkBanned
		}
	}
	return monitor, nil
}

// subscription is one netlink multicast subscription. netlink closes updates
// when its reader stops, which it does on any receive error: ENOBUFS when a
// burst of route changes overflows the socket, such as installing a large
// auto_route exclusion list. A closed channel is always ready, so it has to be
// noticed and replaced rather than selected on again.
type subscription[T any] struct {
	updates chan T
	done    chan struct{}
}

func subscribe[T any](fn func(chan<- T, <-chan struct{}) error) (subscription[T], error) {
	s := subscription[T]{updates: make(chan T, 2), done: make(chan struct{})}
	if err := fn(s.updates, s.done); err != nil {
		close(s.done)
		return subscription[T]{}, err
	}
	return s, nil
}

// stop releases the subscription's socket; netlink closes it once done closes.
func (s *subscription[T]) stop() {
	if s.done != nil {
		close(s.done)
	}
	*s = subscription[T]{}
}

type netlinkSubscriptions struct {
	route   subscription[netlink.RouteUpdate]
	link    subscription[netlink.LinkUpdate]
	address subscription[netlink.AddrUpdate]
}

// resubscribe restores whichever subscriptions have ended,
// stopping at the first that cannot be restored.
func (s *netlinkSubscriptions) resubscribe() (err error) {
	if s.route.updates == nil {
		if s.route, err = subscribe(netlink.RouteSubscribe); err != nil {
			return E.Cause(err, "subscribe route updates")
		}
	}
	if s.link.updates == nil {
		if s.link, err = subscribe(netlink.LinkSubscribe); err != nil {
			return E.Cause(err, "subscribe link updates")
		}
	}
	if s.address.updates == nil {
		if s.address, err = subscribe(netlink.AddrSubscribe); err != nil {
			return E.Cause(err, "subscribe address updates")
		}
	}
	return nil
}

func (s *netlinkSubscriptions) stop() {
	s.route.stop()
	s.link.stop()
	s.address.stop()
}

const (
	resubscribeMin = time.Second
	resubscribeMax = 30 * time.Second
)

func (m *networkUpdateMonitor) Start() error {
	var subs netlinkSubscriptions
	if err := subs.resubscribe(); err != nil {
		subs.stop()
		return err
	}
	go m.loopUpdate(&subs, time.Second)
	return nil
}

func (m *networkUpdateMonitor) loopUpdate(subs *netlinkSubscriptions, minDuration time.Duration) {
	defer subs.stop()
	timer := time.NewTimer(minDuration)
	timer.Stop()
	defer timer.Stop()
	retry := time.NewTimer(resubscribeMin)
	retry.Stop()
	defer retry.Stop()
	var (
		timerC  <-chan time.Time
		retryC  <-chan time.Time
		pending bool
		backoff = resubscribeMin
	)
	// lost handles a subscription whose reader stopped. Updates it would have
	// carried are gone, so the state is treated as changed.
	lost := func() {
		if retryC == nil {
			retry.Reset(backoff)
			retryC = retry.C
		}
	}
	for {
		select {
		case <-m.close:
			return
		case _, ok := <-subs.route.updates:
			if !ok {
				subs.route.stop()
				lost()
			}
		case _, ok := <-subs.link.updates:
			if !ok {
				subs.link.stop()
				lost()
			}
		case _, ok := <-subs.address.updates:
			if !ok {
				subs.address.stop()
				lost()
			}
		case <-retryC:
			retryC = nil
			if err := subs.resubscribe(); err != nil {
				m.logger.Warn(E.Cause(err, "network update monitor"))
				backoff = min(2*backoff, resubscribeMax)
				lost()
				continue
			}
			backoff = resubscribeMin
		case <-timerC:
			if pending {
				m.emit()
				pending = false
				timer.Reset(minDuration)
				continue
			}
			timerC = nil
			continue
		}
		if timerC != nil {
			pending = true
			continue
		}
		m.emit()
		timer.Reset(minDuration)
		timerC = timer.C
	}
}

func (m *networkUpdateMonitor) Close() error {
	select {
	case <-m.close:
		return os.ErrClosed
	default:
	}
	close(m.close)
	return nil
}
