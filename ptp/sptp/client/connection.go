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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/facebook/time/dscp"
	"github.com/facebook/time/timestamp"
)

// UDPConnNoTS describes the functionality we expect from a UDP connection
type UDPConnNoTS interface {
	WriteTo(b []byte, addr unix.Sockaddr) (int, error)
	ReadPacketBuf(buf []byte) (int, netip.Addr, error)
	Close() error
}

// UDPConnWithTS describes the functionality we expect from a UDP connection that will allow us to read TX timestamps
type UDPConnWithTS interface {
	WriteToWithTS(b []byte, src, dst unix.Sockaddr, seq uint16) (time.Time, error)
	ReadPacketBuf(buf, oob []byte) (int, int, unix.Sockaddr, error)
	RXTimestamp(oob []byte, boob int) (time.Time, error)
	Close() error
	ConnFd() int
}

// UDPConn is a wrapper around udp connection and a corresponding fd
type UDPConn struct {
	connFd int
}

// NewUDPConn initialises a new struct UDPConn
func NewUDPConn(address net.IP, port int) (*UDPConn, error) {
	connFd, err := listenUDP(address, port)
	if err != nil {
		return nil, err
	}
	return &UDPConn{
		connFd: connFd,
	}, nil
}

// WriteTo writes bytes to addr via underlying UDPConn
func (c *UDPConn) WriteTo(b []byte, addr unix.Sockaddr) (int, error) {
	return 0, unix.Sendto(c.connFd, b, 0, addr)
}

// ReadPacketBuf reads bytes from underlying fd
func (c *UDPConn) ReadPacketBuf(buf []byte) (int, netip.Addr, error) {
	n, saddr, err := unix.Recvfrom(c.connFd, buf, 0)
	if err != nil {
		return 0, netip.Addr{}, err
	}

	return n, timestamp.SockaddrToAddr(saddr), err
}

// Close closes underlying fd
func (c *UDPConn) Close() error {
	return unix.Close(c.connFd)
}

// UDPConnTS is a wrapper around udp connection and a corresponding fd
type UDPConnTS struct {
	UDPConn

	l           sync.Mutex
	newerKernel bool
	// ifIndex names the egress interface in a pktinfo, which a zero would override.
	// Inet6Pktinfo declares it uint32 and Inet4Pktinfo int32 on Linux, hence both.
	// On non-Linux platforms (darwin, solaris, z/OS) Inet4Pktinfo.Ifindex is
	// uint32, so setInet4PktinfoIfindex does the per-platform conversion, see
	// pktinfo_linux.go and pktinfo_notlinux.go.
	ifIndex  uint32
	ifIndex4 int32
}

// ConfigPktInfo enables pktinfo on the socket so the destination address of
// received packets can be recovered. Both the IPv6 and IPv4 options are set;
// on a single-family socket one of them can legitimately fail (e.g. ENOPROTOOPT
// on an IPv6-only socket), so we only return an error when neither could be enabled.
func ConfigPktInfo(fd int) error {
	err6 := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1)
	err4 := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_PKTINFO, 1)
	if err6 != nil && err4 != nil {
		return fmt.Errorf("enabling pktinfo (IPv6: %w, IPv4: %w)", err6, err4)
	}

	return nil
}

// NewUDPConnTS initialises a new struct UDPConnTS
func NewUDPConnTS(address net.IP, port int, ts timestamp.Timestamp, iface *net.Interface, dscpValue int) (*UDPConnTS, error) {
	if iface.Index <= 0 || iface.Index > math.MaxInt32 {
		return nil, fmt.Errorf("interface index %d out of range", iface.Index)
	}
	udpConn, err := NewUDPConn(address, port)
	if err != nil {
		return nil, err
	}
	if err = dscp.Enable(udpConn.connFd, address, dscpValue); err != nil {
		return nil, fmt.Errorf("setting DSCP on event socket: %w", err)
	}

	// we need to enable HW or SW timestamps on event port
	if err := timestamp.EnableTimestamps(ts, udpConn.connFd, iface); err != nil {
		return nil, fmt.Errorf("failed to enable timestamps on port %d: %w", port, err)
	}

	return &UDPConnTS{
		UDPConn:     *udpConn,
		ifIndex:     uint32(iface.Index),
		ifIndex4:    int32(iface.Index),
		newerKernel: true, // assume kernel is recent enough to support SCM_TS_OPT_ID
	}, nil
}

// ConnFd returns the underlying file descriptor for the connection.
// This is useful for operations like joining multicast groups.
func (c *UDPConnTS) ConnFd() int {
	return c.connFd
}

// WriteToWithTS writes bytes to addr via underlying UDPConn. Uses the Sequence ID for
// reliable matching of HW TX timestamps with socket control messages returned in the
// socket error queue by the kernel (if supported by kernel). A non-nil src pins the source.
func (c *UDPConnTS) WriteToWithTS(b []byte, src, addr unix.Sockaddr, seq uint16) (time.Time, error) {
	c.l.Lock()
	defer c.l.Unlock()

	if c.newerKernel {
		hwts, err := c.sendMsgSeqIDTS(b, src, addr, seq)
		if err != nil {
			// a pinned source can also make sendmsg return EINVAL, and reading that
			// as missing SCM_TS_OPT_ID would drop seq-ID matching for every later send
			if !errors.Is(err, unix.EINVAL) || src != nil {
				return time.Time{}, fmt.Errorf("failed to send message to %v: %w", addr, err)
			}
			c.newerKernel = false
		} else {
			return hwts, nil
		}
	}
	hwts, err := c.sendMsgTS(b, src, addr)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to send message to %v: %w", addr, err)
	}
	return hwts, nil
}

// pktInfoCmsg pins the source address, empty for a nil src
func (c *UDPConnTS) pktInfoCmsg(src unix.Sockaddr) ([]byte, error) {
	switch src := src.(type) {
	case nil:
		return nil, nil
	case *unix.SockaddrInet4:
		return pktInfo4Cmsg(src, c.ifIndex4), nil
	case *unix.SockaddrInet6:
		return pktInfo6Cmsg(src, c.ifIndex), nil
	}
	return nil, fmt.Errorf("unsupported source address type %T", src)
}

func (c *UDPConnTS) sendMsgSeqIDTS(b []byte, src, addr unix.Sockaddr, seq uint16) (time.Time, error) {
	oob, err := c.pktInfoCmsg(src)
	if err != nil {
		return time.Time{}, err
	}
	seqID := uint32(seq)
	soob := make([]byte, unix.CmsgSpace(timestamp.SizeofSeqID))
	timestamp.SeqIDSocketControlMessage(seqID, soob)
	if err := unix.Sendmsg(c.connFd, b, append(oob, soob...), addr, 0); err != nil {
		return time.Time{}, fmt.Errorf("message sent to socket failed: %w", err)
	}
	toob := make([]byte, timestamp.ControlSizeBytes)
	hwts, _, err := timestamp.ReadTimeStampSeqIDBuf(c.connFd, toob, seqID)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to read TX timestamp: %w", err)
	}
	return hwts, nil
}

func (c *UDPConnTS) sendMsgTS(b []byte, src, addr unix.Sockaddr) (time.Time, error) {
	oob, err := c.pktInfoCmsg(src)
	if err != nil {
		return time.Time{}, err
	}
	if err := unix.Sendmsg(c.connFd, b, oob, addr, 0); err != nil {
		return time.Time{}, fmt.Errorf("message sent to socket failed: %w", err)
	}
	hwts, _, err := timestamp.ReadTXtimestamp(c.connFd)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get timestamp of last packet: %w", err)
	}
	return hwts, nil
}

func pktInfo6Cmsg(addr *unix.SockaddrInet6, ifIndex uint32) []byte {
	var socketControlMessageHeaderOffset = binary.Size(unix.Cmsghdr{})
	b := make([]byte, unix.CmsgSpace(unix.SizeofInet6Pktinfo))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.IPPROTO_IPV6
	h.Type = unix.IPV6_PKTINFO
	h.SetLen(unix.CmsgLen(unix.SizeofInet6Pktinfo))
	pktInfo := (*unix.Inet6Pktinfo)(unsafe.Pointer(&b[socketControlMessageHeaderOffset]))
	copy(pktInfo.Addr[:], addr.Addr[:])
	pktInfo.Ifindex = ifIndex
	return b
}

func pktInfo4Cmsg(addr *unix.SockaddrInet4, ifIndex int32) []byte {
	var socketControlMessageHeaderOffset = binary.Size(unix.Cmsghdr{})
	b := make([]byte, unix.CmsgSpace(unix.SizeofInet4Pktinfo))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.IPPROTO_IP
	h.Type = unix.IP_PKTINFO
	h.SetLen(unix.CmsgLen(unix.SizeofInet4Pktinfo))
	pktInfo := (*unix.Inet4Pktinfo)(unsafe.Pointer(&b[socketControlMessageHeaderOffset]))
	copy(pktInfo.Addr[:], addr.Addr[:])
	setInet4PktinfoIfindex(pktInfo, ifIndex)
	return b
}

// ReadPacketBuf reads a packet and its control messages from the underlying fd
func (c *UDPConnTS) ReadPacketBuf(buf, oob []byte) (int, int, unix.Sockaddr, error) {
	return timestamp.ReadPacketWithCMsgBuf(c.connFd, buf, oob)
}

// RXTimestamp parses the hardware RX timestamp out of control messages already read
func (c *UDPConnTS) RXTimestamp(oob []byte, boob int) (time.Time, error) {
	return timestamp.ReadRXTimestamp(oob, boob)
}

func listenUDP(address net.IP, port int) (int, error) {
	domain := unix.AF_INET6
	if address.To4() != nil {
		domain = unix.AF_INET
	}
	// create a UDP socket
	connFd, err := unix.Socket(domain, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return 0, fmt.Errorf("unable to create connection: %w", err)
	}
	if err = unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return 0, fmt.Errorf("setting SO_REUSEPORT on socket: %w", err)
	}
	// set the connection to blocking mode, otherwise recvmsg will just return with nothing most of the time
	if err := unix.SetNonblock(connFd, false); err != nil {
		return 0, fmt.Errorf("failed to set event socket to blocking: %w", err)
	}
	// bind the socket to the address + port
	localAddr := timestamp.IPToSockaddr(address, port)
	if err := unix.Bind(connFd, localAddr); err != nil {
		return 0, fmt.Errorf("unable to bind %v connection: %w", localAddr, err)
	}
	return connFd, nil
}
