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

func requireEqualNetAddrSockAddr(t *testing.T, n net.Addr, s unix.Sockaddr) {
	uaddr := n.(*net.UDPAddr)
	saddr6, ok := s.(*unix.SockaddrInet6)
	if ok {
		require.Equal(t, uaddr.IP.To16(), net.IP(saddr6.Addr[:]))
		require.Equal(t, uaddr.Port, saddr6.Port)
		return
	}
	saddr4 := s.(*unix.SockaddrInet4)
	require.Equal(t, uaddr.IP.To4(), net.IP(saddr4.Addr[:]))
	require.Equal(t, uaddr.Port, saddr4.Port)
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

func TestSockaddrToIP(t *testing.T) {
	ip4 := net.ParseIP("127.0.0.1")
	ip6 := net.ParseIP("::1")
	port := 123

	sa4 := IPToSockaddr(ip4, port)
	sa6 := IPToSockaddr(ip6, port)

	require.Equal(t, ip4.String(), SockaddrToIP(sa4).String())
	require.Equal(t, ip6.String(), SockaddrToIP(sa6).String())
}

func TestSockaddrToPort(t *testing.T) {
	ip4 := net.ParseIP("127.0.0.1")
	ip6 := net.ParseIP("::1")
	port := 123

	sa4 := IPToSockaddr(ip4, port)
	sa6 := IPToSockaddr(ip6, port)

	require.Equal(t, port, SockaddrToPort(sa4))
	require.Equal(t, port, SockaddrToPort(sa6))
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
	require.Equal(t, uint32(7), ScopeID(linkLocal.Addr()))
	require.Zero(t, ScopeID(netip.MustParseAddr("::1")))
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

func TestNewSockaddrWithPort(t *testing.T) {
	oldSA := &unix.SockaddrInet4{Addr: [4]byte{1, 2, 3, 4}, Port: 4567}
	newSA := NewSockaddrWithPort(oldSA, 8901)
	newSA4 := newSA.(*unix.SockaddrInet4)
	require.Equal(t, oldSA.Addr, newSA4.Addr)
	require.Equal(t, 8901, newSA4.Port)
	// changing the original should not change the new one
	oldSA.Addr[0] = 42
	require.NotEqual(t, oldSA.Addr, newSA4.Addr)
}
