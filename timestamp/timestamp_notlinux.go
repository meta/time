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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var timestamping = unix.SO_TIMESTAMP

// Here we have basic HW and SW timestamping support

// byteToTime converts bytes into a timestamp
func byteToTime(data []byte) (time.Time, error) {
	// freebsd supports only SO_TIMESTAMP mode, which returns timeval
	timeval := (*unix.Timeval)(unsafe.Pointer(&data[0]))
	return time.Unix(timeval.Unix()), nil
}

/*
scmDataToTime parses SocketControlMessage Data field into time.Time.
*/
func scmDataToTime(data []byte) (ts time.Time, err error) {
	size := binary.Size(unix.Timeval{})

	ts, err = byteToTime(data[0:size])
	if err != nil {
		return ts, err
	}
	if ts.UnixNano() == 0 {
		return ts, fmt.Errorf("got zero timestamp")
	}

	return ts, nil
}

// socketControlMessageTimestamp parses timestamp from control message
func socketControlMessageTimestamp(b []byte, _ int) (time.Time, error) {
	return scmDataToTime(b[unix.CmsgSpace(0):])
}

// socketControlMessageDrops is not implemented for darwin
func socketControlMessageDrops(_ []byte, _ int) uint32 {
	return 0
}

// socketControlMessageDstAddr parses SocketControlMessage Data to find the Destination Address
func socketControlMessageDstAddr(_ []byte, _ int) (net.IP, error) {
	return nil, unix.ENOTSUP
}

// EnableSWTimestampsRx enables SW RX timestamps on the socket
func EnableSWTimestampsRx(connFd int) error {
	// Allow reading of SW timestamps via socket
	return unix.SetsockoptInt(connFd, unix.SOL_SOCKET, timestamping, 1)
}

// EnableTimestamps enables timestamps on the socket based on requested type
func EnableTimestamps(ts Timestamp, connFd int, _ *net.Interface) error {
	switch ts {
	case SW:
		if err := EnableSWTimestampsRx(connFd); err != nil {
			return fmt.Errorf("cannot enable software timestamps: %w", err)
		}
	default:
		return fmt.Errorf("unrecognized timestamp type: %s", ts)
	}
	return nil
}

// EnableRXQueueOverflow is not implemented for darwin
func EnableRXQueueOverflow(_ int) error {
	return fmt.Errorf("RX queue overflow is not supported on this platform")
}

// ReadTXtimestamp is not supported on this platform.
func ReadTXtimestamp(_ int) (time.Time, int, error) {
	return time.Time{}, 0, errors.New("TX timestamping is not supported on this platform")
}

// ReadTXtimestampBuf is not supported on this platform.
func ReadTXtimestampBuf(_ int, _, _ []byte) (time.Time, int, error) {
	return time.Time{}, 0, errors.New("TX timestamping is unsupported on this platform")
}

// SeqIDSocketControlMessage is not supported on this platform.
func SeqIDSocketControlMessage(_ uint32, _ []byte) {
	// no-op on this platform
}

// ReadTimeStampSeqIDBuf is not supported on this platform.
func ReadTimeStampSeqIDBuf(_ int, _ []byte, _ uint32) (time.Time, int, error) {
	return time.Time{}, 0, errors.New("timestamp sequence ID control messages are unsupported on this platform")
}

// EnableHWTimestamps is not supported on this platform.
func EnableHWTimestamps(_ int, _ *net.Interface) error {
	return errors.New("hardware timestamping is not supported on this platform")
}

// ReportSkippedTX is not supported on this platform.
func ReportSkippedTX(_ int) error {
	return errors.New("TX timestamping is not supported on this platform")
}

// EnableHWTimestampsRx is not supported on this platform.
func EnableHWTimestampsRx(_ int, _ *net.Interface) error {
	return errors.New("hardware RX timestamping is not supported on this platform")
}
