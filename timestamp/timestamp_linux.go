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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// unix.Cmsghdr size differs depending on platform
var socketControlMessageHeaderOffset = binary.Size(unix.Cmsghdr{})

var timestamping = unix.SO_TIMESTAMPING_NEW

func init() {
	// if kernel is older than 5, it doesn't support unix.SO_TIMESTAMPING_NEW
	var uname unix.Utsname
	if err := unix.Uname(&uname); err == nil {
		if uname.Release[0] < '5' {
			// reading such timestamps on 32bit machines will not work, but we can't support everything
			timestamping = unix.SO_TIMESTAMPING
		}
	}
}

/*
scmDataToTime parses SocketControlMessage Data field into time.Time.
The structure can return up to three timestamps. This is a legacy
feature. Only one field is non-zero at any time. Most timestamps
are passed in ts[0]. Hardware timestamps are passed in ts[2].
*/
func scmDataToTime(data []byte) (ts time.Time, err error) {
	// 2 x 64bit ints
	size := 16
	// first, try to use hardware timestamps
	ts, err = byteToTime(data[size*2 : size*3])
	if err != nil {
		return ts, err
	}
	// if hw timestamps aren't present, use software timestamps
	// we can't use ts.IsZero because for some crazy reason timestamp parsed using time.Unix()
	// reports IsZero() == false, even if seconds and nanoseconds are zero.
	if ts.UnixNano() == 0 {
		ts, err = byteToTime(data[0:size])
		if err != nil {
			return ts, err
		}
		if ts.UnixNano() == 0 {
			return ts, fmt.Errorf("got zero timestamp")
		}
	}

	return ts, nil
}

// scmDataToSeqID parses SocketControlMessage Data field to get the SequenceID
func scmDataToSeqID(data []byte) (seqID uint32, err error) {
	se := (*unix.SockExtendedErr)(unsafe.Pointer(&data[0]))
	if unix.Errno(se.Errno) != unix.ENOMSG {
		return 0, fmt.Errorf("expected ENOMSG but got %w", unix.Errno(se.Errno))
	}
	return se.Data, nil
}

// byteToTime converts bytes into a timestamp
func byteToTime(data []byte) (time.Time, error) {
	// __kernel_timespec from linux/time_types.h
	// can't use unix.Timespec which is old timespec that uses 32bit ints on 386 platform.
	sec := *(*int64)(unsafe.Pointer(&data[0]))
	nsec := *(*int64)(unsafe.Pointer(&data[8]))
	return time.Unix(sec, nsec), nil
}

func ioctlHWTimestampCaps(fd int, ifname string) (int32, error) {
	var rxFilter int32

	hw, err := unix.IoctlGetEthtoolTsInfo(fd, ifname)
	if err != nil {
		return 0, fmt.Errorf("failed to run ioctl SIOCETHTOOL to see what is supported: (%w)", err)
	}

	if hw.Tx_types&(1<<unix.HWTSTAMP_TX_ON) == 0 {
		return 0, fmt.Errorf("hardware TX timestamping is not supported for the interface %s", ifname)
	}

	if hw.Rx_filters&(1<<unix.HWTSTAMP_FILTER_PTP_V2_L4_EVENT) > 0 {
		rxFilter = unix.HWTSTAMP_FILTER_PTP_V2_L4_EVENT
	} else if hw.Rx_filters&(1<<unix.HWTSTAMP_FILTER_PTP_V2_EVENT) > 0 {
		rxFilter = unix.HWTSTAMP_FILTER_PTP_V2_EVENT
	} else if hw.Rx_filters&(1<<unix.HWTSTAMP_FILTER_ALL) > 0 {
		rxFilter = unix.HWTSTAMP_FILTER_ALL
	}

	if rxFilter == 0 {
		return 0, fmt.Errorf("hardware RX timestamping is not supported for the interface %s", ifname)
	}
	return rxFilter, nil
}

func ioctlTimestamp(fd int, ifname string, filter int32) error {
	hw, err := unix.IoctlGetHwTstamp(fd, ifname)
	if errors.Is(err, unix.ENOTSUP) {
		// for the loopback interface
		hw = &unix.HwTstampConfig{}
	} else if err != nil {
		return fmt.Errorf("failed to run ioctl SIOCGHWTSTAMP to see what is enabled: %w", err)
	}

	// now check if it matches what we want
	if hw.Tx_type == unix.HWTSTAMP_TX_ON && hw.Rx_filter == filter {
		return nil
	}
	// set to desired values
	hw.Tx_type = unix.HWTSTAMP_TX_ON
	hw.Rx_filter = filter
	if err := unix.IoctlSetHwTstamp(fd, ifname, hw); err != nil {
		return fmt.Errorf("failed to run ioctl SIOCSHWTSTAMP to set timestamps enabled: %w", err)
	}
	return nil
}

// EnableSWTimestampsRx enables SW RX timestamps on the socket
func EnableSWTimestampsRx(connFd int) error {
	flags := unix.SOF_TIMESTAMPING_RX_SOFTWARE |
		unix.SOF_TIMESTAMPING_SOFTWARE
	// Allow reading of SW timestamps via socket
	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, flags)
}

// EnableRXQueueOverflow enables SO_RXQ_OVFL socket option to receive
// drop count in control messages
func EnableRXQueueOverflow(connFd int) error {
	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1)
}

// EnableSWTimestamps enables SW timestamps (TX and RX) on the socket
func EnableSWTimestamps(connFd int) error {
	flags := unix.SOF_TIMESTAMPING_TX_SOFTWARE |
		unix.SOF_TIMESTAMPING_RX_SOFTWARE |
		unix.SOF_TIMESTAMPING_SOFTWARE |
		unix.SOF_TIMESTAMPING_OPT_TSONLY // Makes the kernel return the timestamp as a cmsg alongside an empty packet, as opposed to alongside the original packet.
	// Allow reading of SW timestamps via socket
	if err := unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, flags); err != nil {
		return err
	}

	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_SELECT_ERR_QUEUE, 1)
}

// EnableHWTimestamps enables HW timestamps (TX and RX) on the socket
func EnableHWTimestamps(connFd int, iface *net.Interface) error {
	rxFilter, err := ioctlHWTimestampCaps(connFd, iface.Name)
	if err != nil {
		return err
	}
	if err := ioctlTimestamp(connFd, iface.Name, rxFilter); err != nil {
		return err
	}

	// Enable hardware timestamp capabilities on socket
	flags := unix.SOF_TIMESTAMPING_TX_HARDWARE |
		unix.SOF_TIMESTAMPING_RX_HARDWARE |
		unix.SOF_TIMESTAMPING_RAW_HARDWARE |
		// Makes the kernel return the timestamp as a cmsg with a unique ID in second cmsg to identify the SYNC packet that the timestamp is associated with
		unix.SOF_TIMESTAMPING_OPT_ID |
		// Makes the kernel return the timestamp as a cmsg alongside an empty packet, as opposed to alongside the original packet
		unix.SOF_TIMESTAMPING_OPT_TSONLY
	// Allow reading of HW timestamps via socket
	if err := unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, flags); err != nil {
		return err
	}

	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_SELECT_ERR_QUEUE, 1)
}

// EnableHWTimestampsRx enables HW RX timestamps on the socket
func EnableHWTimestampsRx(connFd int, iface *net.Interface) error {
	rxFilter, err := ioctlHWTimestampCaps(connFd, iface.Name)
	if err != nil {
		return err
	}
	if err := ioctlTimestamp(connFd, iface.Name, rxFilter); err != nil {
		return err
	}

	// Enable hardware timestamp capabilities on socket
	flags := unix.SOF_TIMESTAMPING_RX_HARDWARE |
		unix.SOF_TIMESTAMPING_RAW_HARDWARE // Allow reading of HW timestamps via socket
	if err := unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, flags); err != nil {
		return err
	}

	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_SELECT_ERR_QUEUE, 1)
}

// ReportSkippedTX makes the kernel report packets the NIC sent without a hardware TX timestamp, so
// ReadTimeStampSeqIDBuf returns ErrTXTSSkipped at once instead of waiting for a timestamp that never comes
func ReportSkippedTX(connFd int) error {
	flags, err := unix.GetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING)
	if err != nil {
		return fmt.Errorf("reading timestamping flags: %w", err)
	}
	// the kernel drops the software TX timestamp whenever the driver takes a hardware one
	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, flags|unix.SOF_TIMESTAMPING_TX_SOFTWARE)
}

// SeqIDSocketControlMessage encodes the SequenceID into a socket control message
// and specifies SCM_TS_OPT_ID as the Control Message Header type
func SeqIDSocketControlMessage(seqID uint32, soob []byte) {
	h := (*unix.Cmsghdr)(unsafe.Pointer(&soob[0]))
	h.Level = unix.SOL_SOCKET
	h.Type = unix.SCM_TS_OPT_ID
	h.SetLen(unix.CmsgLen(SizeofSeqID))
	data := soob[unix.CmsgLen(0):unix.CmsgLen(SizeofSeqID)]
	binary.NativeEndian.PutUint32(data, seqID)
}

// pollFunc matches unix.Ppoll so tests can drive the EINTR restart path
type pollFunc func(fds []unix.PollFd, timeout *unix.Timespec, sigmask *unix.Sigset_t) (int, error)

func waitForHWTS(connFd int) error {
	return pollForHWTS(connFd, unix.Ppoll)
}

// pollForHWTS waits until the TX timestamp is ready, for at most TimeoutTXTS.
// unix.Poll is deliberately not used: it takes whole milliseconds as an int and
// passes anything negative to ppoll as "no timeout", so a lossy conversion there
// would reintroduce the unbounded wait this deadline exists to prevent.
func pollForHWTS(connFd int, poll pollFunc) error {
	fds := []unix.PollFd{{Fd: int32(connFd), Events: unix.POLLERR, Revents: 0}} //nolint:gosec // fd is always small
	// a restarted poll must share one deadline, otherwise every EINTR grants
	// another full TimeoutTXTS and the caller's attempt budget bounds nothing
	deadline := time.Now().Add(TimeoutTXTS)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return syscall.ETIMEDOUT
		}
		timeout := unix.NsecToTimespec(remaining.Nanoseconds())
		n, err := poll(fds, &timeout, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return syscall.ETIMEDOUT
		}
		return nil
	}
}

// recvoob receives only OOB message from the socket
// This is used for TX timestamp read of MSG_ERRQUEUE where we couldn't care less about other data.
// This is partially based on Recvmsg
// https://github.com/golang/go/blob/2ebe77a2fda1ee9ff6fd9a3e08933ad1ebaea039/src/syscall/syscall_linux.go#L647
func recvoob(connFd int, oob []byte) (oobn int, err error) {
	var msg unix.Msghdr
	msg.Control = &oob[0]
	msg.SetControllen(len(oob))
	_, _, e1 := unix.Syscall(unix.SYS_RECVMSG, uintptr(connFd), uintptr(unsafe.Pointer(&msg)), uintptr(unix.MSG_ERRQUEUE)) //nolint:gosec // fd is always small
	if e1 != 0 {
		return 0, e1
	}
	// Controllen is bounded by len(oob) we passed to SetControllen
	n := min(msg.Controllen, uint64(len(oob)))
	return int(n), nil
}

// socketControlMessageTimestamp is a very optimised version of ParseSocketControlMessage
// https://github.com/golang/go/blob/2ebe77a2fda1ee9ff6fd9a3e08933ad1ebaea039/src/syscall/sockcmsg_unix.go#L40
// which only parses the timestamp message type
func socketControlMessageTimestamp(b []byte, boob int) (time.Time, error) {
	mlen := 0
	for i := 0; i < boob; i += unix.CmsgSpace(mlen - unix.SizeofCmsghdr) {
		h := (*unix.Cmsghdr)(unsafe.Pointer(&b[i]))
		mlen = int(h.Len) //#nosec G115
		if mlen == 0 {
			break
		}
		// depending on the kernel version, when we ask for SO_TIMESTAMPING_NEW we still might get messages with type SO_TIMESTAMPING
		if h.Level == unix.SOL_SOCKET && int(h.Type) == unix.SO_TIMESTAMPING_NEW || int(h.Type) == unix.SO_TIMESTAMPING {
			return scmDataToTime(b[i+socketControlMessageHeaderOffset : i+mlen])
		}
	}
	return time.Time{}, ErrNoTimestamp
}

// socketControlMessageDrops parses SocketControlMessage for drops count
// drops are provided by SO_RXQ_OVFL control message when enabled
func socketControlMessageDrops(b []byte, boob int) uint32 {
	mlen := 0
	var drops uint32
	for i := 0; i < boob; i += unix.CmsgSpace(mlen - unix.SizeofCmsghdr) {
		h := (*unix.Cmsghdr)(unsafe.Pointer(&b[i]))
		mlen = int(h.Len) //#nosec G115
		if mlen == 0 {
			break
		}
		if h.Level == unix.SOL_SOCKET && int(h.Type) == unix.SO_RXQ_OVFL {
			drops = binary.NativeEndian.Uint32(b[i+socketControlMessageHeaderOffset : i+mlen])
		}
	}
	return drops
}

// socketControlMessageDstAddr parses SocketControlMessage Data to find the Destination Address
func socketControlMessageDstAddr(b []byte, boob int) (net.IP, error) {
	mlen := 0
	for i := 0; i < boob; i += unix.CmsgSpace(mlen - unix.SizeofCmsghdr) {
		h := (*unix.Cmsghdr)(unsafe.Pointer(&b[i]))
		mlen = int(h.Len) //#nosec G115
		if mlen == 0 || i+mlen > boob {
			break
		}
		if h.Level == unix.IPPROTO_IPV6 && int(h.Type) == unix.IPV6_PKTINFO {
			pktInfo6 := (*unix.Inet6Pktinfo)(unsafe.Pointer(&b[i+socketControlMessageHeaderOffset]))
			return net.IP(pktInfo6.Addr[:]), nil
		}

		if h.Level == unix.IPPROTO_IP && int(h.Type) == unix.IP_PKTINFO {
			pktInfo4 := (*unix.Inet4Pktinfo)(unsafe.Pointer(&b[i+socketControlMessageHeaderOffset]))
			return net.IP(pktInfo4.Addr[:]), nil
		}
	}
	return nil, fmt.Errorf("no destination address")
}

// socketControlMessageSeqIDTimestamp parses SocketControlMessage Data field into time.Time and returns it
func socketControlMessageSeqIDTimestamp(b []byte, tboob int, seqID uint32) (time.Time, error) {
	mlen := 0
	tstamp := time.Time{}
	cseq := uint32(0)
	seqFound := false
	var err error
	for i := 0; i < tboob; i += unix.CmsgSpace(mlen - unix.SizeofCmsghdr) {
		h := (*unix.Cmsghdr)(unsafe.Pointer(&b[i]))
		mlen = int(h.Len) //#nosec G115
		if mlen == 0 {
			break
		}
		if h.Level == unix.SOL_SOCKET && int(h.Type) == unix.SO_TIMESTAMPING_NEW {
			tstamp, err = scmDataToTime(b[i+socketControlMessageHeaderOffset : i+mlen])
			if err != nil {
				return time.Time{}, ErrNoTimestamp
			}
		}
		if h.Level == unix.SOL_IPV6 && int(h.Type) == unix.IPV6_RECVERR ||
			h.Level == unix.SOL_IP && int(h.Type) == unix.IP_RECVERR {
			cseq, err = scmDataToSeqID(b[i+socketControlMessageHeaderOffset : i+mlen])
			if err != nil {
				return time.Time{}, ErrNoTimestamp
			}
			seqFound = true
		}
	}
	if tstamp.IsZero() {
		if seqFound && cseq == seqID {
			return time.Time{}, ErrTXTSSkipped
		}
		return time.Time{}, ErrNoTimestamp
	}
	if cseq == seqID {
		return tstamp, nil
	}
	return time.Time{}, ErrNoTimestamp
}

// ReadTXtimestampBuf returns HW TX timestamp, needs to be provided 2 buffers which all can be re-used after ReadTXtimestampBuf finishes.
func ReadTXtimestampBuf(connFd int, oob, toob []byte) (time.Time, int, error) {
	// Accessing hw timestamp
	var boob int

	txfound := false
	// Sometimes we end up with more than 1 TX TS in the buffer.
	// We need to empty it and completely otherwise we end up with a shifted queue read:
	// Sync is out -> read TS from the previous Sync
	// Because we always perform at least 2 tries we start with 0 so on success we are at 1.
	timeStart := time.Now()
	attempts := 0
	for ; attempts < AttemptsTXTS; attempts++ {
		if !txfound {
			// Wait for the poll event, ignore the error
			_ = waitForHWTS(connFd)
		}

		tboob, err := recvoob(connFd, toob)
		if err != nil {
			// We've already seen the valid TX TS and now we have an empty queue.
			// All good
			if txfound {
				break
			}
			// Keep looking for a valid TX TS
			continue
		}
		// We found a valid TX TS. Still check more if there is a newer one
		txfound = true
		boob = tboob
		copy(oob, toob)
	}

	if !txfound {
		timeout := time.Since(timeStart)
		return time.Time{}, attempts, fmt.Errorf("no TX timestamp found after %d tries (%d ms)", AttemptsTXTS, timeout.Milliseconds())
	}
	timestamp, err := socketControlMessageTimestamp(oob, boob)
	return timestamp, attempts, err
}

// ReadTimeStampSeqIDBuf reads the TX timestamp from MSG_ERRQUEUE by reading socket control messages
// The sequence ID is used to identify the SYNC packet that the timestamp is associated with
func ReadTimeStampSeqIDBuf(connFd int, toob []byte, seqID uint32) (time.Time, int, error) {
	timeStart := time.Now()
	attempts := 0
	for ; attempts < AttemptsTXTS; attempts++ {
		e1 := waitForHWTS(connFd)
		if errors.Is(e1, unix.EINVAL) {
			continue
		}
		tboob, err := recvoob(connFd, toob)
		if err != nil {
			continue
		}

		timestamp, err := socketControlMessageSeqIDTimestamp(toob, tboob, seqID)
		if err == nil || errors.Is(err, ErrTXTSSkipped) {
			return timestamp, attempts, err
		}
	}
	timeout := time.Since(timeStart)
	return time.Time{}, attempts, fmt.Errorf("no TX timestamp found after %d tries (%d ms)", AttemptsTXTS, timeout.Milliseconds())
}

// ReadTXtimestamp returns HW TX timestamp
func ReadTXtimestamp(connFd int) (time.Time, int, error) {
	// Accessing hw timestamp
	oob := make([]byte, ControlSizeBytes)
	// TMP buffers
	toob := make([]byte, ControlSizeBytes)

	return ReadTXtimestampBuf(connFd, oob, toob)
}

// EnableTimestamps enables timestamps on the socket based on requested type
func EnableTimestamps(ts Timestamp, connFd int, iface *net.Interface) error {
	switch ts {
	case HW:
		if err := EnableHWTimestamps(connFd, iface); err != nil {
			return fmt.Errorf("cannot enable hardware timestamps: %w", err)
		}
	case HWRX:
		if err := EnableHWTimestampsRx(connFd, iface); err != nil {
			return fmt.Errorf("cannot enable hardware rx timestamps: %w", err)
		}
	case SW:
		if err := EnableSWTimestamps(connFd); err != nil {
			return fmt.Errorf("cannot enable software timestamps: %w", err)
		}
	case SWRX:
		if err := EnableSWTimestampsRx(connFd); err != nil {
			return fmt.Errorf("cannot enable software rx timestamps: %w", err)
		}
	default:
		return fmt.Errorf("unrecognized timestamp type: %s", ts)
	}
	return nil
}
