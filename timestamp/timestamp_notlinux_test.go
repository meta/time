//go:build !linux

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
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func Test_byteToTime(t *testing.T) {
	timeb := []byte{145, 131, 229, 97, 0, 0, 0, 0, 203, 178, 14, 0, 0, 0, 0, 0}
	res, err := byteToTime(timeb)
	require.Nil(t, err)

	tv := &unix.Timeval{Sec: 1642431377, Usec: 963275}
	require.Equal(t, time.Unix(tv.Unix()).UnixNano(), res.UnixNano())
}

func TestEnableSWTimestampsRx(t *testing.T) {
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	connFd, err := ConnFd(conn)
	require.NoError(t, err)

	// Allow reading of kernel timestamps via socket
	err = EnableSWTimestampsRx(connFd)
	require.NoError(t, err)

	// Check that socket option is set
	kernelTimestampsEnabled, err := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMP)
	require.NoError(t, err)

	// To be enabled must be > 0
	require.Greater(t, kernelTimestampsEnabled, 0, "Kernel timestamps are not enabled")
}

func TestEnableTimestamps(t *testing.T) {
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	connFd, err := ConnFd(conn)
	require.NoError(t, err)

	// SOFTWARE
	// Allow reading of kernel timestamps via socket
	err = EnableTimestamps(SW, connFd, &net.Interface{Name: "lo", Index: 1})
	require.NoError(t, err)

	// Check that socket option is set
	kernelTimestampsEnabled, err := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMP)
	require.NoError(t, err)

	// To be enabled must be > 0
	require.Greater(t, kernelTimestampsEnabled, 0, "Kernel timestamps are not enabled")

	// HARDWARE
	err = EnableTimestamps(HW, connFd, &net.Interface{Name: "lo", Index: 1})
	require.Equal(t, fmt.Errorf("unrecognized timestamp type: %s", HW), err)
}

func TestReadPacketWithRXTimestamp(t *testing.T) {
	request := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 42}
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	// get connection file descriptor
	connFd, err := ConnFd(conn)
	require.NoError(t, err)

	// Allow reading of kernel timestamps via socket
	err = EnableSWTimestampsRx(connFd)
	require.NoError(t, err)

	err = unix.SetNonblock(connFd, false)
	require.NoError(t, err)

	// Send a client request
	timeout := 1 * time.Second
	cconn, err := net.DialTimeout("udp", conn.LocalAddr().String(), timeout)
	require.NoError(t, err)
	defer cconn.Close()
	_, err = cconn.Write(request)
	require.NoError(t, err)

	// read kernel timestamp from incoming packet
	data, returnaddr, nowKernelTimestamp, err := ReadPacketWithRXTimestamp(connFd)
	require.NoError(t, err)
	require.Equal(t, request, data, "We should have the same request arriving on the server")
	require.Equal(t, time.Now().Unix()/10, nowKernelTimestamp.Unix()/10, "kernel timestamps should be within 10s")
	requireEqualNetAddrAddrPort(t, cconn.LocalAddr(), returnaddr)
}
