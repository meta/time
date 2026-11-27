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

package client

import (
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/facebook/time/timestamp"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestListenUDPPinsInterface(t *testing.T) {
	fd, err := listenUDP(net.ParseIP("::"), 0, "lo")
	require.NoError(t, err)
	defer unix.Close(fd)

	dev, err := unix.GetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	require.NoError(t, err)
	require.Equal(t, "lo", dev)
}

func TestListenUDPUnknownInterface(t *testing.T) {
	_, err := listenUDP(net.ParseIP("::"), 0, "nosuchiface0")
	require.ErrorIs(t, err, unix.ENODEV)
}

func TestListenUDPPinnedSocketsShareReusePortGroup(t *testing.T) {
	const sockets = 4
	const packets = 100
	fds := make([]int, 0, sockets)
	port := 0
	for range sockets {
		fd, err := listenUDP(net.ParseIP("::1"), port, "lo")
		require.NoError(t, err)
		defer unix.Close(fd)
		require.NoError(t, unix.SetNonblock(fd, true))
		if port == 0 {
			sa, err := unix.Getsockname(fd)
			require.NoError(t, err)
			port = sa.(*unix.SockaddrInet6).Port
		}
		fds = append(fds, fd)
	}

	// a fresh source port per packet, as one flow always hashes to one socket
	for range packets {
		c, err := net.Dial("udp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		require.NoError(t, err)
		_, err = c.Write([]byte{0})
		require.NoError(t, err)
		require.NoError(t, c.Close())
	}

	got := make([]int, sockets)
	total := 0
	buf := make([]byte, 8)
	deadline := time.Now().Add(2 * time.Second)
	for total < packets && time.Now().Before(deadline) {
		for i, fd := range fds {
			if _, _, err := unix.Recvfrom(fd, buf, 0); err == nil {
				got[i]++
				total++
			}
		}
	}
	require.Equal(t, packets, total)
	for i, n := range got {
		require.Less(t, n, packets, "socket %d took every packet, the group collapsed: %v", i, got)
	}
}

// stampedConnPair returns a UDPConnTS with software RX stamping and a socket that sends to it
func stampedConnPair(tb testing.TB) (*UDPConnTS, int, unix.Sockaddr) {
	tb.Helper()
	fd, err := listenUDP(net.ParseIP("::1"), 0, "lo")
	require.NoError(tb, err)
	require.NoError(tb, timestamp.EnableSWTimestampsRx(fd))
	conn := &UDPConnTS{UDPConn: UDPConn{connFd: fd}}
	tb.Cleanup(func() { conn.Close() })
	local, err := unix.Getsockname(fd)
	require.NoError(tb, err)
	clientFd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	require.NoError(tb, err)
	tb.Cleanup(func() { unix.Close(clientFd) })
	return conn, clientFd, local
}

func TestReadPacketBuf(t *testing.T) {
	conn, clientFd, local := stampedConnPair(t)
	buf := make([]byte, timestamp.PayloadSizeBytes)
	oob := make([]byte, timestamp.ControlSizeBytes)

	require.NoError(t, unix.Sendto(clientFd, buf[:44], 0, local))
	n, _, from, err := conn.ReadPacketBuf(buf, oob)
	require.NoError(t, err)
	require.Equal(t, 44, n)
	require.Equal(t, netip.IPv6Loopback(), from.Addr())
}

func BenchmarkReadPacketBuf(b *testing.B) {
	conn, clientFd, local := stampedConnPair(b)
	buf := make([]byte, timestamp.PayloadSizeBytes)
	oob := make([]byte, timestamp.ControlSizeBytes)
	b.ReportAllocs()
	for b.Loop() {
		_ = unix.Sendto(clientFd, buf[:44], 0, local)
		_, _, _, _ = conn.ReadPacketBuf(buf, oob)
	}
}
