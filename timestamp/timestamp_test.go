/*
Copyright (c) Facebook, Inc. and its affiliates.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package timestamp

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func requireEqualNetAddrAddrPort(t *testing.T, n net.Addr, from netip.AddrPort) {
	require.Equal(t, n.(*net.UDPAddr).AddrPort(), netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
}

func TestConnFd(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	connfd, err := ConnFd(conn)
	require.NoError(t, err)
	require.Greater(t, connfd, 0, "connection fd must be > 0")
}

func TestIPToSockaddr(t *testing.T) {
	ip4 := net.ParseIP("127.0.0.1")
	ip6 := net.ParseIP("::1")
	port := 123

	expectedSA4 := &unix.SockaddrInet4{Port: port}
	copy(expectedSA4.Addr[:], ip4.To4())

	expectedSA6 := &unix.SockaddrInet6{Port: port}
	copy(expectedSA6.Addr[:], ip6.To16())

	sa4 := IPToSockaddr(ip4, port)
	sa6 := IPToSockaddr(ip6, port)

	require.Equal(t, expectedSA4, sa4)
	require.Equal(t, expectedSA6, sa6)
}

func TestAddrToSockaddr(t *testing.T) {
	ip4 := netip.MustParseAddr("192.168.0.1")
	ip6 := netip.MustParseAddr("::1")
	port := 123

	expectedSA4 := &unix.SockaddrInet4{Port: port}
	copy(expectedSA4.Addr[:], ip4.AsSlice())

	expectedSA6 := &unix.SockaddrInet6{Port: port}
	copy(expectedSA6.Addr[:], ip6.AsSlice())

	sa4 := AddrToSockaddr(ip4, port)
	sa6 := AddrToSockaddr(ip6, port)

	require.Equal(t, expectedSA4, sa4)
	require.Equal(t, expectedSA6, sa6)
}

func TestSockaddrToAddrPort(t *testing.T) {
	for _, s := range []string{"127.0.0.1:123", "[::1]:123", "[::ffff:10.0.0.1]:320"} {
		want := netip.MustParseAddrPort(s)
		require.Equal(t, want, SockaddrToAddrPort(AddrToSockaddr(want.Addr(), int(want.Port()))), s)
	}
	require.Equal(t, netip.AddrPort{}, SockaddrToAddrPort(&unix.SockaddrUnix{Name: "/tmp/sock"}))
	require.Equal(t, netip.AddrPort{}, SockaddrToAddrPort(&unix.SockaddrInet4{Port: 1 << 16}))

	linkLocal := SockaddrToAddrPort(&unix.SockaddrInet6{Addr: [16]byte{0xfe, 0x80, 15: 1}, Port: 319, ZoneId: 7})
	require.Equal(t, netip.MustParseAddrPort("[fe80::1%7]:319"), linkLocal)
}

func TestSockaddrBuf(t *testing.T) {
	var b SockaddrBuf
	require.Equal(t, &unix.SockaddrInet4{Addr: [4]byte{10, 0, 0, 1}, Port: 319}, b.Sockaddr(netip.MustParseAddrPort("10.0.0.1:319")))
	require.Equal(t, &unix.SockaddrInet6{Addr: [16]byte{15: 1}, Port: 320}, b.Sockaddr(netip.MustParseAddrPort("[::1]:320")))
	// a link-local client is only reachable through the interface it came from
	require.Equal(t, &unix.SockaddrInet6{Addr: [16]byte{0xfe, 0x80, 15: 1}, Port: 319, ZoneId: 7}, b.Sockaddr(netip.MustParseAddrPort("[fe80::1%7]:319")))
	require.Equal(t, &unix.SockaddrInet6{Addr: [16]byte{15: 1}, Port: 319}, b.Sockaddr(netip.MustParseAddrPort("[::1]:319")), "zone must not leak into the next destination")
	// a scope round-trips through a read and a reply
	sa := &unix.SockaddrInet6{Addr: [16]byte{0xfe, 0x80, 15: 1}, Port: 319, ZoneId: 7}
	require.Equal(t, sa, b.Sockaddr(SockaddrToAddrPort(sa)))
}

func TestSockaddrBufZeroAlloc(t *testing.T) {
	var b SockaddrBuf
	addrs := []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:319"), netip.MustParseAddrPort("[fe80::1%7]:320")}
	require.Zero(t, testing.AllocsPerRun(100, func() {
		for _, a := range addrs {
			_ = b.Sockaddr(a)
		}
	}))
}

func BenchmarkSockaddrBuf(b *testing.B) {
	var buf SockaddrBuf
	addr := netip.MustParseAddrPort("[2001:db8::1]:319")
	b.ReportAllocs()
	for b.Loop() {
		_ = buf.Sockaddr(addr)
	}
}

func TestTimestampUnmarshalText(t *testing.T) {
	var ts Timestamp
	require.Equal(t, "timestamp", ts.Type())

	err := ts.UnmarshalText([]byte("hardware"))
	require.NoError(t, err)
	require.Equal(t, HW, ts)
	require.Equal(t, HW.String(), ts.String())

	err = ts.UnmarshalText([]byte("hardware_rx"))
	require.NoError(t, err)
	require.Equal(t, HWRX, ts)
	require.Equal(t, HWRX.String(), ts.String())

	err = ts.UnmarshalText([]byte("software"))
	require.NoError(t, err)
	require.Equal(t, SW, ts)
	require.Equal(t, SW.String(), ts.String())

	err = ts.UnmarshalText([]byte("software_rx"))
	require.NoError(t, err)
	require.Equal(t, SWRX, ts)
	require.Equal(t, SWRX.String(), ts.String())

	err = ts.UnmarshalText([]byte("nope"))
	require.Equal(t, errors.New("unknown timestamp type \"nope\""), err)
	// Check we didn't change the value
	require.Equal(t, SWRX, ts)
}

func TestTimestampMarshalText(t *testing.T) {
	text, err := HW.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "hardware", string(text))

	text, err = HWRX.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "hardware_rx", string(text))

	text, err = SW.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "software", string(text))

	text, err = SWRX.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "software_rx", string(text))

	require.Equal(t, Unsupported, Timestamp(42).String())
	text, err = Timestamp(42).MarshalText()
	require.Equal(t, errors.New("unknown timestamp type \"Unsupported\""), err)
	require.Equal(t, "Unsupported", string(text))
}
