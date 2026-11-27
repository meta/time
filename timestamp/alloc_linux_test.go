//go:build !asan

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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// ASAN forces unsafe.Pointer conversions onto the heap, so these only build without it

// TestReadTimeStampSeqIDBufZeroAlloc keeps the per-Sync TX timestamp read allocation free
func TestReadTimeStampSeqIDBufZeroAlloc(t *testing.T) {
	fd, sa := txTimestampSocket(t)
	toob := make([]byte, ControlSizeBytes)
	buf := make([]byte, 64)
	var seq uint32
	require.Zero(t, testing.AllocsPerRun(100, func() {
		require.NoError(t, unix.Sendto(fd, buf[:1], 0, sa))
		_, _, err := ReadTimeStampSeqIDBuf(fd, toob, seq)
		require.NoError(t, err)
		_, _ = unix.Read(fd, buf)
		seq++
	}))
}

// TestReadPacketWithCMsgAddrPortZeroAlloc keeps the per-DELAY_REQ receive allocation free
func TestReadPacketWithCMsgAddrPortZeroAlloc(t *testing.T) {
	connFd, clientFd, serverAddr := rxSocketPair(t, "udp6")
	buf := make([]byte, PayloadSizeBytes)
	oob := make([]byte, ControlSizeBytes)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		require.NoError(t, unix.Sendto(clientFd, buf[:44], 0, serverAddr))
		_, _, _, err := ReadPacketWithCMsgAddrPort(connFd, buf, oob)
		require.NoError(t, err)
	}))
}

// TestReadPacketWithCMsgAddrPortNoOOBZeroAlloc keeps the general-port receive allocation free
func TestReadPacketWithCMsgAddrPortNoOOBZeroAlloc(t *testing.T) {
	connFd, clientFd, serverAddr := rxSocketPair(t, "udp4")
	buf := make([]byte, PayloadSizeBytes)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		require.NoError(t, unix.Sendto(clientFd, buf[:44], 0, serverAddr))
		_, _, _, err := ReadPacketWithCMsgAddrPort(connFd, buf, nil)
		require.NoError(t, err)
	}))
}

// TestReadPacketWithRXTimestampBufZeroAlloc keeps the per-request NTP receive allocation free
func TestReadPacketWithRXTimestampBufZeroAlloc(t *testing.T) {
	connFd, clientFd, serverAddr := rxSocketPair(t, "udp6")
	buf := make([]byte, PayloadSizeBytes)
	oob := make([]byte, ControlSizeBytes)
	// resend until stamped: packets that arrive before the kernel turns RX stamping on carry no timestamp
	require.Eventually(t, func() bool {
		require.NoError(t, unix.Sendto(clientFd, buf[:48], 0, serverAddr))
		_, _, _, err := ReadPacketWithRXTimestampBuf(connFd, buf, oob)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		require.NoError(t, unix.Sendto(clientFd, buf[:48], 0, serverAddr))
		_, _, _, err := ReadPacketWithRXTimestampBuf(connFd, buf, oob)
		require.NoError(t, err)
	}))
}
