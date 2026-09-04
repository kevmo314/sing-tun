//go:build with_gvisor

package tun

import (
	"net/netip"

	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
)

var _ stack.LinkEndpoint = (*LinkEndpointFilter)(nil)

type LinkEndpointFilter struct {
	stack.LinkEndpoint
	BroadcastAddress netip.Addr
	Writer           GVisorTun
}

func (w *LinkEndpointFilter) Attach(dispatcher stack.NetworkDispatcher) {
	w.LinkEndpoint.Attach(&networkDispatcherFilter{dispatcher, w.BroadcastAddress, w.Writer})
}

var _ stack.NetworkDispatcher = (*networkDispatcherFilter)(nil)

type networkDispatcherFilter struct {
	stack.NetworkDispatcher
	broadcastAddress netip.Addr
	writer           GVisorTun
}

func (w *networkDispatcherFilter) DeliverNetworkPacket(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	var network header.Network
	if protocol == header.IPv4ProtocolNumber {
		if headerPackets, loaded := pkt.Data().PullUp(header.IPv4MinimumSize); loaded {
			network = header.IPv4(headerPackets)
		}
	} else {
		if headerPackets, loaded := pkt.Data().PullUp(header.IPv6MinimumSize); loaded {
			network = header.IPv6(headerPackets)
		}
	}
	if network == nil {
		w.NetworkDispatcher.DeliverNetworkPacket(protocol, pkt)
		return
	}
	destination := AddrFromAddress(network.DestinationAddress())
	if destination == w.broadcastAddress || !destination.IsGlobalUnicast() {
		w.writer.WritePacket(pkt)
		return
	}
	w.NetworkDispatcher.DeliverNetworkPacket(protocol, pkt)
}

// The embedded interface promotes only LinkEndpoint's methods, so without
// these the stack cannot see the wrapped endpoint's segmentation offload and
// every TCP segment leaves at MSS size.
func (w *LinkEndpointFilter) GSOMaxSize() uint32 {
	if gso, ok := w.LinkEndpoint.(stack.GSOEndpoint); ok {
		return gso.GSOMaxSize()
	}
	return 0
}

func (w *LinkEndpointFilter) SupportedGSO() stack.SupportedGSO {
	if gso, ok := w.LinkEndpoint.(stack.GSOEndpoint); ok {
		return gso.SupportedGSO()
	}
	return stack.GSONotSupported
}
