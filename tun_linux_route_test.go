//go:build linux

package tun

import (
	"net"
	"net/netip"
	"runtime"
	"slices"
	"testing"

	"github.com/sagernet/netlink"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const testRouteTable = 2022

// enterTestNetns moves the test goroutine's locked thread into a fresh network
// namespace with a dummy link, so route tests never touch the host namespace.
// Run as root, or unprivileged via `unshare -rn go test ...`.
func enterTestNetns(tb testing.TB) netlink.Link {
	tb.Helper()
	runtime.LockOSThread()
	origin, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		tb.Skip("get netns: ", err)
	}
	// Unshare directly: netns.New switches namespaces before opening its handle,
	// so a failure there could leave this thread in the new namespace.
	if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
		origin.Close()
		runtime.UnlockOSThread()
		tb.Skip("unshare netns (needs CAP_SYS_ADMIN): ", err)
	}
	tb.Cleanup(func() {
		if netns.Set(origin) == nil {
			runtime.UnlockOSThread()
		}
		origin.Close()
	})
	err = netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "rt0"}})
	if err != nil {
		tb.Fatal(err)
	}
	link, err := netlink.LinkByName("rt0")
	if err != nil {
		tb.Fatal(err)
	}
	if err = netlink.LinkSetUp(link); err != nil {
		tb.Fatal(err)
	}
	return link
}

// testRoutes returns count routes starting at index start, alternating
// IPv4 /24 and IPv6 /48 destinations.
func testRoutes(link netlink.Link, start, count int) []netlink.Route {
	routes := make([]netlink.Route, 0, count)
	for i := start; i < start+count; i++ {
		var prefix netip.Prefix
		if i%2 == 0 {
			n := i / 2
			prefix = netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(n >> 8), byte(n), 0}), 24)
		} else {
			n := i / 2
			prefix = netip.PrefixFrom(netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(n >> 8), byte(n)}), 48)
		}
		routes = append(routes, netlink.Route{
			Dst:       prefixToIPNet(prefix),
			LinkIndex: link.Attrs().Index,
			Table:     testRouteTable,
		})
	}
	return routes
}

func installedRoutes(tb testing.TB) map[string]bool {
	tb.Helper()
	list, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: testRouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		tb.Fatal(err)
	}
	installed := make(map[string]bool, len(list))
	for _, route := range list {
		installed[route.Dst.String()] = true
	}
	return installed
}

func TestUpdateRoutes(t *testing.T) {
	link := enterTestNetns(t)
	handle := newRouteHandle()
	defer handle.Close()

	oldRoutes := testRoutes(link, 0, 200)
	for i := range oldRoutes {
		if err := handle.RouteAdd(&oldRoutes[i]); err != nil {
			t.Fatal(err)
		}
	}
	newRoutes := testRoutes(link, 100, 200)
	if err := updateRoutes(handle, oldRoutes, newRoutes, nil); err != nil {
		t.Fatal(err)
	}
	installed := installedRoutes(t)
	if len(installed) != len(newRoutes) {
		t.Fatalf("expected %d routes, got %d", len(newRoutes), len(installed))
	}
	for _, route := range newRoutes {
		if !installed[route.Dst.String()] {
			t.Fatalf("missing route %s", route.Dst)
		}
	}

	// Unchanged set is a no-op.
	if err := updateRoutes(handle, newRoutes, newRoutes, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(installedRoutes(t)); got != len(newRoutes) {
		t.Fatalf("expected %d routes, got %d", len(newRoutes), got)
	}
}

// listInstalled reads back the routes on link in the test table, as
// UpdateRouteOptions does.
func listInstalled(tb testing.TB, handle *netlink.Handle, link netlink.Link) []netlink.Route {
	tb.Helper()
	list, err := handle.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{
		LinkIndex: link.Attrs().Index, Table: testRouteTable,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TABLE)
	if err != nil {
		tb.Fatal(err)
	}
	return list
}

func TestUpdateRoutesRepairsInstalledState(t *testing.T) {
	link := enterTestNetns(t)
	handle := newRouteHandle()
	defer handle.Close()

	wanted := testRoutes(link, 0, 100)
	// Default routes come back from the kernel without a destination; they must
	// still match the configured 0.0.0.0/0 and ::/0 rather than be replaced.
	for _, prefix := range []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")} {
		wanted = append(wanted, netlink.Route{Dst: prefixToIPNet(prefix), LinkIndex: link.Attrs().Index, Table: testRouteTable})
	}
	for i := range wanted {
		if err := handle.RouteAdd(&wanted[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Lose some routes behind the update's back, as a partial failure or a
	// manual flush would.
	for i := range 40 {
		if err := handle.RouteDel(&wanted[i]); err != nil {
			t.Fatal(err)
		}
	}
	installed := make(map[routeKey]struct{})
	for _, route := range listInstalled(t, handle, link) {
		installed[routeKeyOf(&route)] = struct{}{}
	}
	if err := updateRoutes(handle, wanted, wanted, installed); err != nil {
		t.Fatal(err)
	}
	after := listInstalled(t, handle, link)
	if len(after) != len(wanted) {
		t.Fatalf("expected %d routes after repair, got %d", len(wanted), len(after))
	}
	keys := make(map[routeKey]bool, len(after))
	for i := range after {
		keys[routeKeyOf(&after[i])] = true
	}
	for i := range wanted {
		if !keys[routeKeyOf(&wanted[i])] {
			t.Fatalf("route %s missing after repair", wanted[i].Dst)
		}
	}
}

func TestUpdateRoutesOnlyRemovesOwnRoutes(t *testing.T) {
	link := enterTestNetns(t)
	handle := newRouteHandle()
	defer handle.Close()

	defaultRoute := netlink.Route{Dst: prefixToIPNet(netip.MustParsePrefix("0.0.0.0/0")), LinkIndex: link.Attrs().Index, Table: testRouteTable}
	oldRoutes := append(testRoutes(link, 0, 10), defaultRoute)
	foreign := netlink.Route{Dst: prefixToIPNet(netip.MustParsePrefix("192.0.2.0/24")), LinkIndex: link.Attrs().Index, Table: testRouteTable}
	for _, route := range append(slices.Clone(oldRoutes), foreign) {
		if err := handle.RouteAdd(&route); err != nil {
			t.Fatal(err)
		}
	}
	// Narrower prefixes replace the old default route.
	newRoutes := testRoutes(link, 0, 10)
	if err := updateRoutes(handle, oldRoutes, newRoutes, nil); err != nil {
		t.Fatal(err)
	}
	installed := installedRoutes(t)
	if installed["0.0.0.0/0"] || installed["<nil>"] {
		t.Fatal("old default route was left installed")
	}
	if !installed["192.0.2.0/24"] {
		t.Fatal("a route this instance did not configure was removed")
	}
	if len(installed) != len(newRoutes)+1 {
		t.Fatalf("expected %d routes, got %d", len(newRoutes)+1, len(installed))
	}
}

func TestUpdateRoutesDoesNotOverwriteOtherInterfaces(t *testing.T) {
	link := enterTestNetns(t)
	handle := newRouteHandle()
	defer handle.Close()
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "rt1"}}); err != nil {
		t.Fatal(err)
	}
	other, err := netlink.LinkByName("rt1")
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(other); err != nil {
		t.Fatal(err)
	}
	prefix := prefixToIPNet(netip.MustParsePrefix("198.51.100.0/24"))
	foreign := netlink.Route{Dst: prefix, LinkIndex: other.Attrs().Index, Table: testRouteTable}
	if err = handle.RouteAdd(&foreign); err != nil {
		t.Fatal(err)
	}
	wanted := []netlink.Route{{Dst: prefix, LinkIndex: link.Attrs().Index, Table: testRouteTable}}
	if err = updateRoutes(handle, nil, wanted, map[routeKey]struct{}{}); err == nil {
		t.Fatal("expected a conflict with another interface's route to be reported")
	}
	list, err := handle.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: testRouteTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].LinkIndex != other.Attrs().Index {
		t.Fatalf("another interface's route was overwritten: %+v", list)
	}
}

func TestRouteKeyOf(t *testing.T) {
	a := netlink.Route{Dst: &net.IPNet{IP: net.IPv4(1, 2, 3, 0), Mask: net.CIDRMask(24, 32)}, Table: 1, LinkIndex: 2}
	b := netlink.Route{Dst: &net.IPNet{IP: net.IP{1, 2, 3, 0}, Mask: net.CIDRMask(24, 32)}, Table: 1, LinkIndex: 2}
	if routeKeyOf(&a) != routeKeyOf(&b) {
		t.Fatal("4-in-6 and 4-byte destinations must map to the same key")
	}
	b.Gw = net.IP{1, 2, 3, 1}
	if routeKeyOf(&a) == routeKeyOf(&b) || routeKeyOf(&a).routeDstKey != routeKeyOf(&b).routeDstKey {
		t.Fatal("gateway must only affect the full key")
	}
}

func benchmarkRoutes(b *testing.B, handle func() *netlink.Handle) {
	link := enterTestNetns(b)
	h := handle()
	defer h.Close()
	routes := testRoutes(link, 0, 10000)
	for b.Loop() {
		for i := range routes {
			if err := h.RouteAdd(&routes[i]); err != nil {
				b.Fatal(err)
			}
		}
		for i := range routes {
			if err := h.RouteDel(&routes[i]); err != nil && err != unix.ESRCH {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkRoutesPackage adds and removes 10k routes with the package-level
// netlink functions (the zero Handle, one socket per request).
func BenchmarkRoutesPackage(b *testing.B) {
	benchmarkRoutes(b, func() *netlink.Handle { return &netlink.Handle{} })
}

// BenchmarkRoutesHandle adds and removes 10k routes over one reused socket.
func BenchmarkRoutesHandle(b *testing.B) {
	benchmarkRoutes(b, newRouteHandle)
}
