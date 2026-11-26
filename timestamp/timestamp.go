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

// Here we have basic HW and SW timestamping support

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// ControlSizeBytes is a socket control message containing TX/RX timestamp
	// If the read fails we may endup with multiple timestamps in the buffer
	// which is best to read right away. Currently we read 104 bytes per packet
	ControlSizeBytes = 128
	// PayloadSizeBytes is a size of maximum ptp packet which is usually up to 66 bytes
	PayloadSizeBytes = 128
	// look only for X sequential TS
	defaultTXTS = 100
	// SizeofSeqID is the size of the sequence ID field in bytes
	SizeofSeqID = 0x4 // 4 bytes
)

// Timestamp is a type of timestamp
type Timestamp int

const (
	// SW is a software timestamp
	SW Timestamp = iota
	// SWRX is a software RX timestamp
	SWRX
	// HW is a hardware timestamp
	HW
	// HWRX is a hardware RX timestamp
	HWRX
)

// Unsupported is a string for unsupported timestamp
const Unsupported = "Unsupported"

// timestampToString is a map from Timestamp to string
var timestampToString = map[Timestamp]string{
	SW:   "software",
	SWRX: "software_rx",
	HW:   "hardware",
	HWRX: "hardware_rx",
}

// ErrNoTimestamp is returned when no timestamp is found in the socket control message
var ErrNoTimestamp = errors.New("failed to find timestamp in socket control message")

// ErrTXTSSkipped is returned when the NIC sent the packet without a TX timestamp, see ReportSkippedTX
var ErrTXTSSkipped = errors.New("packet was sent without a TX timestamp")

// ErrNoTimestampForMulticastPkt is returned when no timestamp is found in the socket control message and destination address is multicast
var ErrNoTimestampForMulticastPkt = errors.New("failed to find timestamp in socket control message (multicast packet)")

// MarshalText timestamp to byte slice
func (t Timestamp) MarshalText() ([]byte, error) {
	_, ok := timestampToString[t]
	if ok {
		return []byte(t.String()), nil
	}
	return []byte(Unsupported), fmt.Errorf("unknown timestamp type %q", Unsupported)
}

// String timestamp to string
func (t Timestamp) String() string {
	v, ok := timestampToString[t]
	if ok {
		return v
	}
	return Unsupported
}

// timestampFromString returns channel from string
func timestampFromString(value string) (*Timestamp, error) {
	for k, v := range timestampToString {
		if v == value {
			return &k, nil
		}
	}
	return nil, fmt.Errorf("unknown timestamp type %q", value)
}

// UnmarshalText timestamp from byte slice
func (t *Timestamp) UnmarshalText(value []byte) error {
	return t.Set(string(value))
}

// Set timestamp from string
func (t *Timestamp) Set(value string) error {
	ts, err := timestampFromString(value)
	if err != nil {
		return err
	}
	*t = *ts
	return nil
}

// Type is required by the cobra.Value interface
func (t *Timestamp) Type() string {
	return "timestamp"
}

// AttemptsTXTS is the configured amount of attempts to read TX timestamp
var AttemptsTXTS = defaultTXTS

// TimeoutTXTS is the configured timeout to read TX timestamp
var TimeoutTXTS = time.Millisecond

// ConnFd returns file descriptor of a connection
func ConnFd(conn *net.UDPConn) (int, error) {
	sc, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	var intfd int
	err = sc.Control(func(fd uintptr) {
		intfd = int(fd)
	})
	if err != nil {
		return -1, err
	}
	return intfd, nil
}

// ReadPacketWithRXTimestamp returns byte packet and HW RX timestamp
func ReadPacketWithRXTimestamp(connFd int) ([]byte, netip.AddrPort, time.Time, error) {
	// Accessing hw timestamp
	buf := make([]byte, PayloadSizeBytes)
	oob := make([]byte, ControlSizeBytes)

	bbuf, from, t, err := ReadPacketWithRXTimestampBuf(connFd, buf, oob)
	if err != nil {
		return nil, netip.AddrPort{}, time.Time{}, err
	}
	return buf[:bbuf], from, t, nil
}

// ReadPacketWithRXTimestampBuf writes byte packet into provide buffer buf, and returns number of bytes copied to the buffer, client ip and HW RX timestamp.
// oob buffer can be reaused after ReadPacketWithRXTimestampBuf call. Reading a packet does not allocate.
func ReadPacketWithRXTimestampBuf(connFd int, buf, oob []byte) (int, netip.AddrPort, time.Time, error) {
	bbuf, boob, from, err := recvmsgAddrPort(connFd, buf, oob)
	if err != nil {
		return 0, netip.AddrPort{}, time.Time{}, fmt.Errorf("failed to read timestamp: %w", err)
	}

	timestamp, err := socketControlMessageTimestamp(oob, boob)
	if errors.Is(err, ErrNoTimestamp) {
		dst, dstErr := socketControlMessageDstAddr(oob, boob)
		if dstErr == nil && dst.IsMulticast() {
			return 0, netip.AddrPort{}, time.Time{}, ErrNoTimestampForMulticastPkt
		}
	}
	if err != nil {
		return 0, netip.AddrPort{}, time.Time{}, err
	}
	return bbuf, from, timestamp, nil
}

// ReadPacketWithCMsgBuf writes byte packet into provided packet and cmsg buffer, and returns number of bytes copied to the buffers
func ReadPacketWithCMsgBuf(connFd int, buf, oob []byte) (int, int, unix.Sockaddr, error) {
	bbuf, boob, _, saddr, err := unix.Recvmsg(connFd, buf, oob, 0)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("failed to read packet: %w", err)
	}
	return bbuf, boob, saddr, nil
}

// ReadPacketWithCMsgAddrPort is ReadPacketWithCMsgBuf for UDP sockets that returns the sender by value,
// so reading a packet does not allocate. An IPv6 scope ID is returned as the numeric zone of the address.
func ReadPacketWithCMsgAddrPort(connFd int, buf, oob []byte) (int, int, netip.AddrPort, error) {
	bbuf, boob, addr, err := recvmsgAddrPort(connFd, buf, oob)
	if err != nil {
		return 0, 0, netip.AddrPort{}, fmt.Errorf("failed to read packet: %w", err)
	}
	return bbuf, boob, addr, nil
}

// ReadRXTimestamp returns HW timestamp from CMSG buffer
func ReadRXTimestamp(oob []byte, boob int) (time.Time, error) {
	return socketControlMessageTimestamp(oob, boob)
}

// ReadSocketDrops returns number of dropped packets from CMSG buffer
func ReadSocketDrops(oob []byte, boob int) uint32 {
	return socketControlMessageDrops(oob, boob)
}

// IPToSockaddr converts IP + port into a socket address
// Somewhat copy from https://github.com/golang/go/blob/16cd770e0668a410a511680b2ac1412e554bd27b/src/net/ipsock_posix.go#L145
func IPToSockaddr(ip net.IP, port int) unix.Sockaddr {
	if ip.To4() != nil {
		sa := &unix.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip.To4())
		return sa
	}
	sa := &unix.SockaddrInet6{Port: port}
	copy(sa.Addr[:], ip.To16())
	return sa
}

// AddrToSockaddr converts netip.Addr + port into a socket address
func AddrToSockaddr(ip netip.Addr, port int) unix.Sockaddr {
	if ip.Is4() {
		return &unix.SockaddrInet4{Port: port, Addr: ip.As4()}
	}
	return &unix.SockaddrInet6{Port: port, Addr: ip.As16()}
}

// SockaddrToAddr converts socket address to a netip.Addr
// Somewhat copy from https://github.com/golang/go/blob/658b5e66ecbc41a49e6fb5aa63c5d9c804cf305f/src/net/udpsock_posix.go#L15
func SockaddrToAddr(sa unix.Sockaddr) netip.Addr {
	switch sa := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrFrom4(sa.Addr).Unmap()
	case *unix.SockaddrInet6:
		return netip.AddrFrom16(sa.Addr).Unmap()
	}
	return netip.Addr{}
}

// SockaddrToAddrPort converts socket address to a netip.AddrPort, keeping IPv4-mapped IPv6 addresses as they are
func SockaddrToAddrPort(sa unix.Sockaddr) netip.AddrPort {
	var ip netip.Addr
	var port int
	switch sa := sa.(type) {
	case *unix.SockaddrInet4:
		ip, port = netip.AddrFrom4(sa.Addr), sa.Port
	case *unix.SockaddrInet6:
		ip, port = addrFrom16(sa.Addr, sa.ZoneId), sa.Port
	}
	if !ip.IsValid() || port < 0 || port > math.MaxUint16 {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(ip, uint16(port))
}

// addrFrom16 keeps the scope of a link-local sender as a numeric zone, so replies can reach it
func addrFrom16(ip [16]byte, scope uint32) netip.Addr {
	addr := netip.AddrFrom16(ip)
	if scope == 0 {
		return addr
	}
	return addr.WithZone(strconv.FormatUint(uint64(scope), 10))
}

// scopeID returns the IPv6 scope kept by addrFrom16, or 0 if addr has none
func scopeID(addr netip.Addr) uint32 {
	zone := addr.Zone()
	if zone == "" {
		return 0
	}
	id, _ := strconv.ParseUint(zone, 10, 32)
	return uint32(id)
}

// SockaddrBuf converts addresses to a unix.Sockaddr without allocating. It holds one destination
// per address family, so a returned Sockaddr is only valid until the next call.
type SockaddrBuf struct {
	in4 unix.SockaddrInet4
	in6 unix.SockaddrInet6
}

// Sockaddr returns addr as a unix.Sockaddr, keeping the scope of a link-local address
func (b *SockaddrBuf) Sockaddr(addr netip.AddrPort) unix.Sockaddr {
	ip := addr.Addr()
	if ip.Is4() {
		b.in4 = unix.SockaddrInet4{Addr: ip.As4(), Port: int(addr.Port())}
		return &b.in4
	}
	b.in6 = unix.SockaddrInet6{Addr: ip.As16(), Port: int(addr.Port()), ZoneId: scopeID(ip)}
	return &b.in6
}

// SockaddrToPort converts socket address to an IP
// Somewhat copy from https://github.com/golang/go/blob/658b5e66ecbc41a49e6fb5aa63c5d9c804cf305f/src/net/udpsock_posix.go#L15
func SockaddrToPort(sa unix.Sockaddr) int {
	switch sa := sa.(type) {
	case *unix.SockaddrInet4:
		return sa.Port
	case *unix.SockaddrInet6:
		return sa.Port
	}
	return 0
}

// NewSockaddrWithPort creates a new socket address with the same IP and new port
func NewSockaddrWithPort(sa unix.Sockaddr, port int) unix.Sockaddr {
	switch sa := sa.(type) {
	case *unix.SockaddrInet4:
		return &unix.SockaddrInet4{Addr: sa.Addr, Port: port}
	case *unix.SockaddrInet6:
		return &unix.SockaddrInet6{Addr: sa.Addr, Port: port}
	}
	return nil
}

// JoinMulticast joins a multicast group on the given connection.
// It supports both IPv4 and IPv6 multicast addresses.
func JoinMulticast(connFd int, iface *net.Interface, multicastAddr net.IP) error {
	// linux kernel stores ifindex as int32 and interface index can't be negative
	if iface.Index <= 0 || iface.Index > math.MaxInt32 {
		return fmt.Errorf("invalid interface index %d for multicast", iface.Index)
	}

	if multicastAddr.To4() != nil {
		// IPv4 multicast - use IPMreqn for reliable interface binding
		mreqn := &unix.IPMreqn{
			Ifindex: int32(iface.Index),
		}
		copy(mreqn.Multiaddr[:], multicastAddr.To4())

		if err := unix.SetsockoptIPMreqn(connFd, unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, mreqn); err != nil {
			return fmt.Errorf("joining IPv4 multicast group %s: %w", multicastAddr, err)
		}
	} else {
		// IPv6 multicast
		mreq := &unix.IPv6Mreq{
			Interface: uint32(iface.Index),
		}
		copy(mreq.Multiaddr[:], multicastAddr.To16())

		if err := unix.SetsockoptIPv6Mreq(connFd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, mreq); err != nil {
			return fmt.Errorf("joining IPv6 multicast group %s: %w", multicastAddr, err)
		}
	}
	return nil
}
