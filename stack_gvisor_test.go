//go:build with_gvisor

package tun

import (
	"errors"
	"testing"

	"github.com/sagernet/gvisor/pkg/tcpip/link/channel"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
)

type startupEndpoint struct {
	*channel.Endpoint
	onAttach func()
}

func (e *startupEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	if dispatcher != nil {
		e.onAttach()
	}
	e.Endpoint.Attach(dispatcher)
}

func TestTransportReadyBeforePacketReadersStart(t *testing.T) {
	initialized := false
	attached := false
	ep := &startupEndpoint{Endpoint: channel.New(1, 1500, ""), onAttach: func() {
		attached = true
		if !initialized {
			t.Error("packet readers started before transport initialization")
		}
	}}
	s, err := newGVisorStack(ep, stack.NICOptions{}, false, true, func(*stack.Stack) error {
		initialized = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !attached {
		t.Fatal("endpoint was never attached")
	}
}

func TestTransportFailureDoesNotStartPacketReaders(t *testing.T) {
	ep := &startupEndpoint{Endpoint: channel.New(1, 1500, ""), onAttach: func() {
		t.Error("endpoint attached after transport initialization failed")
	}}
	want := errors.New("transport startup failed")
	s, err := newGVisorStack(ep, stack.NICOptions{}, false, true, func(*stack.Stack) error {
		return want
	})
	if s != nil {
		s.Close()
		t.Error("failed initialization returned a stack")
	}
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}
