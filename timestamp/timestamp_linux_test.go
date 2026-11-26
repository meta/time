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
	"math"
	"net"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/facebook/time/hostendian"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func reverse(s []byte) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func Test_byteToTime(t *testing.T) {
	timeb := []byte{63, 155, 21, 96, 0, 0, 0, 0, 52, 156, 191, 42, 0, 0, 0, 0}
	if hostendian.IsBigEndian {
		// reverse two int64 individually
		reverse(timeb[0:8])
		reverse(timeb[8:16])
	}
	res, err := byteToTime(timeb)
	require.Nil(t, err)

	require.Equal(t, int64(1612028735717200436), res.UnixNano())
}

func Test_ReadTXtimestamp(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.Nil(t, err)
	defer conn.Close()

	connFd, err := ConnFd(conn)
	require.Nil(t, err)

	err = EnableSWTimestamps(connFd)
	require.Nil(t, err)

	start := time.Now()
	txts, attempts, err := ReadTXtimestamp(connFd)
	duration := time.Since(start)
	require.Equal(t, time.Time{}, txts)
	require.Equal(t, defaultTXTS, attempts)
	errStr := fmt.Sprintf("no TX timestamp found after %d tries", defaultTXTS)
	require.ErrorContains(t, err, errStr)
	require.GreaterOrEqual(t, duration, time.Duration(AttemptsTXTS)*TimeoutTXTS)

	oldAttempts, oldTimeout := AttemptsTXTS, TimeoutTXTS
	t.Cleanup(func() { AttemptsTXTS, TimeoutTXTS = oldAttempts, oldTimeout })
	AttemptsTXTS = 10
	TimeoutTXTS = 5 * time.Millisecond

	start = time.Now()
	txts, attempts, err = ReadTXtimestamp(connFd)
	duration = time.Since(start)
	require.Equal(t, time.Time{}, txts)
	require.Equal(t, 10, attempts)
	errStr = fmt.Sprintf("no TX timestamp found after %d tries", 10)
	require.ErrorContains(t, err, errStr)
	require.GreaterOrEqual(t, duration, time.Duration(AttemptsTXTS)*TimeoutTXTS)

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	_, err = conn.WriteTo([]byte{}, addr)
	require.Nil(t, err)
	txts, attempts, err = ReadTXtimestamp(connFd)

	require.NotEqual(t, time.Time{}, txts)
	require.Equal(t, 1, attempts)
	require.Nil(t, err)
}

func errQueueSocket(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	connFd, err := ConnFd(conn)
	require.NoError(t, err)
	require.NoError(t, EnableSWTimestamps(connFd))
	return connFd
}

func TestWaitForHWTSReportsTimeout(t *testing.T) {
	connFd := errQueueSocket(t)

	oldTimeout := TimeoutTXTS
	t.Cleanup(func() { TimeoutTXTS = oldTimeout })
	TimeoutTXTS = 100 * time.Millisecond

	start := time.Now()
	err := waitForHWTS(connFd)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, syscall.ETIMEDOUT)
	require.GreaterOrEqual(t, elapsed, TimeoutTXTS)
}

func TestPollErrRejectsFdOutsidePollFd(t *testing.T) {
	_, err := pollErr(-1, time.Millisecond)
	require.ErrorIs(t, err, unix.EBADF)
	if strconv.IntSize == 64 {
		tooBig := int64(math.MaxInt32) + 1
		_, err = pollErr(int(tooBig), time.Millisecond)
		require.ErrorIs(t, err, unix.EBADF)
	}
}

func TestWaitForHWTSSharesOneDeadlineAcrossEINTR(t *testing.T) {
	connFd := errQueueSocket(t)

	oldTimeout := TimeoutTXTS
	t.Cleanup(func() { TimeoutTXTS = oldTimeout })
	TimeoutTXTS = 100 * time.Millisecond

	// interrupting far more often than the deadline allows: a loop that shares one
	// deadline gives up part way through, a loop that restarts the timeout never does
	const interrupts = 100
	const perCall = 10 * time.Millisecond
	var budgets []time.Duration
	poll := func(_ int, timeout time.Duration) (int, error) {
		budgets = append(budgets, timeout)
		time.Sleep(perCall)
		if len(budgets) >= interrupts {
			return 0, nil
		}
		return -1, syscall.EINTR
	}

	require.ErrorIs(t, pollForHWTS(connFd, poll), syscall.ETIMEDOUT)

	require.NotEmpty(t, budgets)
	require.Less(t, len(budgets), interrupts)
	require.LessOrEqual(t, budgets[0], TimeoutTXTS)
	for i := 1; i < len(budgets); i++ {
		require.Less(t, budgets[i], budgets[i-1])
	}
}

// txTimestampSocket returns a socket with one SW TX timestamp per send, tagged with OPT_ID like ptp4u's
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

// TestReadTimeStampSeqIDBufZeroAlloc keeps the per-Sync TX timestamp read allocation free
func TestReadTimeStampSeqIDBufZeroAlloc(t *testing.T) {
	fd, sa := txTimestampSocket(t)
	toob := make([]byte, ControlSizeBytes)
	buf := make([]byte, 64)
	var seq uint32
	read := func(t *testing.T) {
		require.NoError(t, unix.Sendto(fd, buf[:1], 0, sa))
		_, _, err := ReadTimeStampSeqIDBuf(fd, toob, seq)
		require.NoError(t, err)
		_, _ = unix.Read(fd, buf)
		seq++
	}
	read(t)
	t.Run("allocations", func(t *testing.T) {
		if asanEnabled {
			t.Skip("ASAN forces unsafe.Pointer conversions to allocate")
		}
		require.Zero(t, testing.AllocsPerRun(100, func() { read(t) }))
	})
}

func BenchmarkReadTimeStampSeqIDBuf(b *testing.B) {
	fd, sa := txTimestampSocket(b)
	toob := make([]byte, ControlSizeBytes)
	buf := make([]byte, 64)
	var seq uint32
	b.ReportAllocs()
	for b.Loop() {
		_ = unix.Sendto(fd, buf[:1], 0, sa)
		_, _, _ = ReadTimeStampSeqIDBuf(fd, toob, seq)
		_, _ = unix.Read(fd, buf)
		seq++
	}
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

func TestReadPacketWithCMsgAddrPort(t *testing.T) {
	for _, network := range []string{"udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			connFd, clientFd, serverAddr := rxSocketPair(t, network)
			clientAddr, err := unix.Getsockname(clientFd)
			require.NoError(t, err)
			request := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 42}
			buf := make([]byte, PayloadSizeBytes)
			oob := make([]byte, ControlSizeBytes)
			var rxTS time.Time
			// resend until stamped: packets that arrive before the kernel turns RX stamping on carry no timestamp
			require.Eventually(t, func() bool {
				require.NoError(t, unix.Sendto(clientFd, request, 0, serverAddr))
				n, oobn, from, err := ReadPacketWithCMsgAddrPort(connFd, buf, oob)
				require.NoError(t, err)
				require.Equal(t, request, buf[:n])
				require.Equal(t, SockaddrToAddrPort(clientAddr), from)
				rxTS, err = ReadRXTimestamp(oob, oobn)
				return err == nil
			}, time.Second, 10*time.Millisecond)
			require.WithinDuration(t, time.Now(), rxTS, 10*time.Second)
		})
	}
}

// TestReadPacketWithCMsgAddrPortZeroAlloc keeps the per-DELAY_REQ receive allocation free
func TestReadPacketWithCMsgAddrPortZeroAlloc(t *testing.T) {
	connFd, clientFd, serverAddr := rxSocketPair(t, "udp6")
	buf := make([]byte, PayloadSizeBytes)
	oob := make([]byte, ControlSizeBytes)
	read := func(t *testing.T) {
		require.NoError(t, unix.Sendto(clientFd, buf[:44], 0, serverAddr))
		_, _, _, err := ReadPacketWithCMsgAddrPort(connFd, buf, oob)
		require.NoError(t, err)
	}
	read(t)
	t.Run("allocations", func(t *testing.T) {
		if asanEnabled {
			t.Skip("ASAN forces unsafe.Pointer conversions to allocate")
		}
		require.Zero(t, testing.AllocsPerRun(100, func() { read(t) }))
	})
}

func BenchmarkReadPacketWithCMsgAddrPort(b *testing.B) {
	connFd, clientFd, serverAddr := rxSocketPair(b, "udp6")
	buf := make([]byte, PayloadSizeBytes)
	oob := make([]byte, ControlSizeBytes)
	b.ReportAllocs()
	for b.Loop() {
		_ = unix.Sendto(clientFd, buf[:44], 0, serverAddr)
		_, _, _, _ = ReadPacketWithCMsgAddrPort(connFd, buf, oob)
	}
}

func BenchmarkReadPacketWithCMsgBuf(b *testing.B) {
	connFd, clientFd, serverAddr := rxSocketPair(b, "udp6")
	buf := make([]byte, PayloadSizeBytes)
	oob := make([]byte, ControlSizeBytes)
	b.ReportAllocs()
	for b.Loop() {
		_ = unix.Sendto(clientFd, buf[:44], 0, serverAddr)
		_, _, _, _ = ReadPacketWithCMsgBuf(connFd, buf, oob)
	}
}

func Test_scmDataToTime(t *testing.T) {
	hwData := []byte{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		63, 155, 21, 96, 0, 0, 0, 0, 52, 156, 191, 42, 0, 0, 0, 0,
	}
	swData := []byte{
		63, 155, 21, 96, 0, 0, 0, 0, 52, 156, 191, 42, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	noData := []byte{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}

	if hostendian.IsBigEndian {
		// make two int64 BigEndian
		reverse(hwData[32:40])
		reverse(hwData[40:48])
		// ditto, but different position of int64s
		reverse(swData[0:8])
		reverse(swData[8:16])
	}

	tests := []struct {
		name    string
		data    []byte
		want    int64
		wantErr bool
	}{
		{
			name:    "hardware timestamp",
			data:    hwData,
			want:    1612028735717200436,
			wantErr: false,
		},
		{
			name:    "software timestamp",
			data:    swData,
			want:    1612028735717200436,
			wantErr: false,
		},
		{
			name:    "zero timestamp",
			data:    noData,
			want:    0,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := scmDataToTime(tt.data)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.Nil(t, err)
				require.Equal(t, tt.want, res.UnixNano())
			}
		})
	}
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
	timestampsEnabled, _ := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING)
	newTimestampsEnabled, _ := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING_NEW)

	// At least one of them should be set, which it > 0
	require.Greater(t, timestampsEnabled+newTimestampsEnabled, 0, "None of the socket options is set")
}

func TestEnableSWTimestamps(t *testing.T) {
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	connFd, err := ConnFd(conn)
	require.NoError(t, err)

	// Allow reading of kernel timestamps via socket
	err = EnableSWTimestamps(connFd)
	require.NoError(t, err)

	// Check that socket option is set
	timestampsEnabled, _ := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING)
	newTimestampsEnabled, _ := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING_NEW)

	// At least one of them should be set, which it > 0
	require.Greater(t, timestampsEnabled+newTimestampsEnabled, 0, "None of the socket options is set")
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
	timestampsEnabled, _ := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING)
	newTimestampsEnabled, _ := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING_NEW)

	// At least one of them should be set, which it > 0
	require.Greater(t, timestampsEnabled+newTimestampsEnabled, 0, "None of the socket options is set")

	// SOFTWARE_RX
	// Allow reading of kernel timestamps via socket
	err = EnableTimestamps(SWRX, connFd, &net.Interface{Name: "lo", Index: 1})
	require.NoError(t, err)

	// Check that socket option is set
	timestampsEnabled, _ = unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING)
	newTimestampsEnabled, _ = unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING_NEW)

	// At least one of them should be set, which it > 0
	require.Greater(t, timestampsEnabled+newTimestampsEnabled, 0, "None of the socket options is set")

	// Unsupported
	err = EnableTimestamps(42, connFd, &net.Interface{Name: "lo", Index: 1})
	require.Equal(t, fmt.Errorf("unrecognized timestamp type: Unsupported"), err)
}

func pktInfo6Cmsg(addr net.IP) []byte {
	b := make([]byte, unix.CmsgSpace(unix.SizeofInet6Pktinfo))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.IPPROTO_IPV6
	h.Type = unix.IPV6_PKTINFO
	h.SetLen(unix.CmsgLen(unix.SizeofInet6Pktinfo))
	pktInfo := (*unix.Inet6Pktinfo)(unsafe.Pointer(&b[socketControlMessageHeaderOffset]))
	copy(pktInfo.Addr[:], addr.To16())
	return b
}

func pktInfo4Cmsg(addr net.IP) []byte {
	b := make([]byte, unix.CmsgSpace(unix.SizeofInet4Pktinfo))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.IPPROTO_IP
	h.Type = unix.IP_PKTINFO
	h.SetLen(unix.CmsgLen(unix.SizeofInet4Pktinfo))
	pktInfo := (*unix.Inet4Pktinfo)(unsafe.Pointer(&b[socketControlMessageHeaderOffset]))
	copy(pktInfo.Addr[:], addr.To4())
	return b
}

func TestSocketControlMessageDstAddr(t *testing.T) {
	t.Run("IPv6 multicast", func(t *testing.T) {
		addr := net.ParseIP("ff0e::181")
		b := pktInfo6Cmsg(addr)
		dst, err := socketControlMessageDstAddr(b, len(b))
		require.NoError(t, err)
		require.True(t, dst.Equal(addr))
		require.True(t, dst.IsMulticast())
	})

	t.Run("IPv6 unicast", func(t *testing.T) {
		addr := net.ParseIP("2401:db00::1")
		b := pktInfo6Cmsg(addr)
		dst, err := socketControlMessageDstAddr(b, len(b))
		require.NoError(t, err)
		require.True(t, dst.Equal(addr))
		require.False(t, dst.IsMulticast())
	})

	t.Run("IPv4 multicast", func(t *testing.T) {
		addr := net.ParseIP("224.0.1.129")
		b := pktInfo4Cmsg(addr)
		dst, err := socketControlMessageDstAddr(b, len(b))
		require.NoError(t, err)
		require.True(t, dst.Equal(addr))
		require.True(t, dst.IsMulticast())
	})

	t.Run("IPv4 unicast", func(t *testing.T) {
		addr := net.ParseIP("10.0.0.1")
		b := pktInfo4Cmsg(addr)
		dst, err := socketControlMessageDstAddr(b, len(b))
		require.NoError(t, err)
		require.True(t, dst.Equal(addr))
		require.False(t, dst.IsMulticast())
	})

	t.Run("no pktinfo", func(t *testing.T) {
		_, err := socketControlMessageDstAddr(make([]byte, 16), 16)
		require.Error(t, err)
	})
}

func TestSocketControlMessageTimestamp(t *testing.T) {
	if timestamping != unix.SO_TIMESTAMPING_NEW {
		t.Skip("This test supports SO_TIMESTAMPING_NEW only. No sample of SO_TIMESTAMPING")
	}

	var b []byte
	var toob int

	// unix.Cmsghdr used in socketControlMessageTimestamp differs depending on platform
	switch runtime.GOARCH {
	case "amd64":
		b = []byte{
			0x40, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x1, 0x0, 0x0, 0x0, 0x41, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x79, 0xab, 0x24, 0x68, 0x0, 0x0, 0x0,
			0x0, 0xfc, 0xab, 0xf9, 0x8, 0x0, 0x0,
			0x0, 0x0, 0x3c, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x29, 0x0, 0x0, 0x0, 0x19, 0x0,
			0x0, 0x0, 0x2a, 0x0, 0x0, 0x0, 0x4, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0,
		}
		toob = len(b)
	default:
		t.Skip("This test checks amd64 platform only")
	}

	ts, err := socketControlMessageTimestamp(b, toob)
	require.NoError(t, err)
	require.Equal(t, int64(1747233657150580220), ts.UnixNano())
}

func TestSocketControlMessageTimestampFail(t *testing.T) {
	if timestamping != unix.SO_TIMESTAMPING_NEW {
		t.Skip("This test supports SO_TIMESTAMPING_NEW only. No sample of SO_TIMESTAMPING")
	}

	_, err := socketControlMessageTimestamp(make([]byte, 16), 16)
	require.ErrorIs(t, ErrNoTimestamp, err)
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
	require.Equal(t, request, data, "We should have the same request arriving on the server")
	require.Equal(t, time.Now().Unix()/10, nowKernelTimestamp.Unix()/10, "kernel timestamps should be within 10s")
	requireEqualNetAddrSockAddr(t, cconn.LocalAddr(), returnaddr)
	require.NoError(t, err)
}

func TestReadPacketWithRXTXTimestamp(t *testing.T) {
	request := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 42}
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	// get connection file descriptor
	connFd, err := ConnFd(conn)
	require.NoError(t, err)

	// Allow reading of kernel timestamps via socket
	err = EnableSWTimestamps(connFd)
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
	requireEqualNetAddrSockAddr(t, cconn.LocalAddr(), returnaddr)

	// send packet and read TX timestamp
	_, err = conn.WriteTo(request, cconn.LocalAddr())
	require.NoError(t, err)
	txts, attempts, err := ReadTXtimestamp(connFd)
	require.NotEqual(t, time.Time{}, txts)
	require.Equal(t, 1, attempts)
	require.Nil(t, err)
}

func TestReadHWTimestampCaps(t *testing.T) {
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	// get connection file descriptor
	connFd, err := ConnFd(conn)
	require.NoError(t, err)

	rxFilters, err := ioctlHWTimestampCaps(connFd, "lo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not supported")
	require.Equal(t, int32(0), rxFilters)
}

func TestScmDataToSeqID(t *testing.T) {
	// 0x2a is ENOMSG - see /usr/include/asm-generic/errno.h
	hwData := []byte{
		0x2a, 0x0, 0x0, 0x0, 0x4, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0xd2, 0x4, 0x0, 0x0,
	}
	seqID, err := scmDataToSeqID(hwData)
	require.NoError(t, err)
	require.Equal(t, uint32(1234), seqID)
}

func TestScmDataToSeqIDErrornoNotENOMSG(t *testing.T) {
	// 0x26 is ENOSYS - see /usr/include/asm-generic/errno.h
	hwData := []byte{
		0x26, 0x0, 0x0, 0x0, 0x4, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0xd2, 0x4, 0x0, 0x0,
	}
	_, err := scmDataToSeqID(hwData)
	require.Error(t, err)
	require.ErrorContains(t, err, "expected ENOMSG but got function not implemented")
}

func TestSeqIDSocketControlMessage(t *testing.T) {
	soob := make([]byte, unix.CmsgSpace(SizeofSeqID))
	seqID := uint32(8765)
	var sockControlMsg []byte
	SeqIDSocketControlMessage(seqID, soob)

	switch runtime.GOARCH {
	case "amd64":
		// Socket Control Message with 20 byte length (0x14), level SOL_SOCKET (0x1)
		// and type SCM_TS_OPT_ID (0x51) and a 4 byte payload (0x3d, 0x22, 0x0, 0x0)
		sockControlMsg = []byte{
			0x14, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x1, 0x0, 0x0, 0x0, 0x51, 0x0, 0x0, 0x0,
			0x3d, 0x22, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, // 4 bytes of padding at the end
		}
	case "386":
		// Socket Control Message with 16 byte length (0x10), level SOL_SOCKET (0x1)
		// and type SCM_TS_OPT_ID (0x51) and a 4 byte payload (0x3d, 0x22, 0x0, 0x0)
		sockControlMsg = []byte{
			0x10, 0x0, 0x0, 0x0, 0x1, 0x0, 0x0, 0x0,
			0x51, 0x0, 0x0, 0x0, 0x3d, 0x22, 0x0, 0x0,
		}
	default:
		t.Skip("This test supports 386/amd64 platform only")
	}
	require.Equal(t, sockControlMsg, soob)
}

func TestSocketControlMessageSeqIDTimestamp(t *testing.T) {
	tboob := 128
	seqID := uint32(3248)
	// byte array of 2 socket control messages: First message includes a timestamp and the second
	// message includes the sequence ID of a Sync packet
	// cmsghdr struct comprises len (8 bytes), level (4 bytes), type (4 bytes) and data (length can vary)
	switch runtime.GOARCH {
	case "amd64":
		sockControlMsgs := []byte{
			0x40, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, // message has len of 64 bytes (0x40), level SOL_SOCKET (0x1), type SO_TIMESTAMPING_NEW (0x41)
			0x1, 0x0, 0x0, 0x0, 0x41, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x69, 0x75, 0x23, 0x68, 0x0, 0x0, 0x0, 0x0,
			0x7b, 0xcb, 0x4, 0x6, 0x0, 0x0, 0x0, 0x0,
			0x3c, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, // message has len of 60 bytes (0x3c) level SOL_IPV6 (0x29) and type IPV6_RECVERR (0x19)
			0x29, 0x0, 0x0, 0x0, 0x19, 0x0, 0x0, 0x0,
			0x2a, 0x0, 0x0, 0x0, 0x4, 0x0, 0x0, 0x0, // sock_extended_err with errno ENOMSG (0x2a) and data field of 4 bytes (0x4) which is the Sequence ID of 3248 (0xb0, 0x0c, 0x0, 0x0)
			0x0, 0x0, 0x0, 0x0, 0xb0, 0x0c, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
			0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, // Last 4 bytes are there to align the socket message to an 8-byte boundary
		}
		ts, err := socketControlMessageSeqIDTimestamp(sockControlMsgs, tboob, seqID)
		require.NoError(t, err)
		require.Equal(t, int64(1747154281100977531), ts.UnixNano())
	default:
		t.Skip("This test supports amd64 platform only")
	}
}

func TestSocketControlMessageRXDrops(t *testing.T) {
	switch runtime.GOARCH {
	case "amd64":
		sockControlMsg := []byte{
			0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // message has len of 20 bytes (0x14), level SOL_SOCKET (0x1), type SO_RXQ_OVFL (0x28)
			0x01, 0x00, 0x00, 0x00, 0x28, 0x00, 0x00, 0x00,
			0x91, 0xd7, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00, // Last 4 bytes are there to align the socket message to an 8-byte boundary
		}
		drops := socketControlMessageDrops(sockControlMsg, 20)
		require.Equal(t, uint32(448401), drops)
	default:
		t.Skip("This test supports amd64 platform only")
	}
}

func recvErrCmsg(seqID uint32) []byte {
	size := int(unsafe.Sizeof(unix.SockExtendedErr{}))
	b := make([]byte, unix.CmsgSpace(size))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.SOL_IP
	h.Type = unix.IP_RECVERR
	h.SetLen(unix.CmsgLen(size))
	se := (*unix.SockExtendedErr)(unsafe.Pointer(&b[socketControlMessageHeaderOffset]))
	se.Errno = uint32(unix.ENOMSG)
	se.Data = seqID
	return b
}

func TestSocketControlMessageSeqIDTimestampSkipped(t *testing.T) {
	b := recvErrCmsg(3248)

	_, err := socketControlMessageSeqIDTimestamp(b, len(b), 3248)
	require.ErrorIs(t, err, ErrTXTSSkipped)

	_, err = socketControlMessageSeqIDTimestamp(b, len(b), 3249)
	require.ErrorIs(t, err, ErrNoTimestamp)
}

func TestReportSkippedTX(t *testing.T) {
	for _, tt := range []struct {
		name    string
		flags   int
		wantErr error
	}{
		{
			name:  "software",
			flags: unix.SOF_TIMESTAMPING_SOFTWARE | unix.SOF_TIMESTAMPING_OPT_ID | unix.SOF_TIMESTAMPING_OPT_TSONLY,
		},
		{
			name: "hardware",
			flags: unix.SOF_TIMESTAMPING_TX_HARDWARE | unix.SOF_TIMESTAMPING_RX_HARDWARE |
				unix.SOF_TIMESTAMPING_RAW_HARDWARE | unix.SOF_TIMESTAMPING_OPT_ID | unix.SOF_TIMESTAMPING_OPT_TSONLY,
			wantErr: ErrTXTSSkipped,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			connFd, err := ConnFd(conn)
			require.NoError(t, err)

			require.NoError(t, unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, tt.flags))
			require.NoError(t, ReportSkippedTX(connFd))

			got, err := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING)
			require.NoError(t, err)
			require.Equal(t, tt.flags|unix.SOF_TIMESTAMPING_TX_SOFTWARE, got)

			for range 2 {
				_, err = conn.WriteTo([]byte{1}, conn.LocalAddr())
				require.NoError(t, err)
			}
			ts, attempts, err := ReadTimeStampSeqIDBuf(connFd, make([]byte, ControlSizeBytes), 1)
			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, tt.wantErr == nil, !ts.IsZero())
			require.Equal(t, 1, attempts)
		})
	}
}
