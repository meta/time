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
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Socket helpers shared by the linux test files. They live here, not in timestamp_linux_test.go,
// because the Fedora package deletes that file on s390x.

func txTimestampSocket(tb testing.TB) (int, unix.Sockaddr) {
	tb.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	require.NoError(tb, err)
	tb.Cleanup(func() { unix.Close(fd) })
	require.NoError(tb, unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	flags := unix.SOF_TIMESTAMPING_TX_SOFTWARE | unix.SOF_TIMESTAMPING_SOFTWARE |
		unix.SOF_TIMESTAMPING_OPT_ID | unix.SOF_TIMESTAMPING_OPT_TSONLY
	require.NoError(tb, unix.SetsockoptInt(fd, unix.SOL_SOCKET, timestamping, flags))
	require.NoError(tb, unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SELECT_ERR_QUEUE, 1))
	// sending to ourselves keeps the receive queue drained by the test, not the kernel
	sa, err := unix.Getsockname(fd)
	require.NoError(tb, err)
	return fd, sa
}

func rxSocketPair(tb testing.TB, network string) (int, int, unix.Sockaddr) {
	tb.Helper()
	lo := "127.0.0.1"
	if network == "udp6" {
		lo = "::1"
	}
	conn, err := net.ListenUDP(network, &net.UDPAddr{IP: net.ParseIP(lo)})
	require.NoError(tb, err)
	tb.Cleanup(func() { conn.Close() })
	connFd, err := ConnFd(conn)
	require.NoError(tb, err)
	require.NoError(tb, EnableSWTimestampsRx(connFd))
	require.NoError(tb, unix.SetNonblock(connFd, false))
	serverAddr, err := unix.Getsockname(connFd)
	require.NoError(tb, err)

	domain := unix.AF_INET
	if network == "udp6" {
		domain = unix.AF_INET6
	}
	clientFd, err := unix.Socket(domain, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	require.NoError(tb, err)
	tb.Cleanup(func() { unix.Close(clientFd) })
	require.NoError(tb, unix.Bind(clientFd, IPToSockaddr(net.ParseIP(lo), 0)))
	return connFd, clientFd, serverAddr
}
