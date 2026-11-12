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
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestPktInfo4Cmsg(t *testing.T) {
	addr := unix.SockaddrInet4{Port: 320, Addr: [4]byte{10, 0, 1, 2}}
	raw := pktInfo4Cmsg(&addr, 7)

	if len(raw) != unix.CmsgSpace(unix.SizeofInet4Pktinfo) {
		t.Errorf("unexpected control message size: %d != %d", len(raw), unix.CmsgSpace(unix.SizeofInet4Pktinfo))
	}

	h := (*unix.Cmsghdr)(unsafe.Pointer(&raw[0]))
	if h.Level != unix.IPPROTO_IP {
		t.Errorf("expected level IPPROTO_IP, got %d", h.Level)
	}
	if h.Type != unix.IP_PKTINFO {
		t.Errorf("expected type IP_PKTINFO, got %d", h.Type)
	}

	pktInfo := (*unix.Inet4Pktinfo)(unsafe.Pointer(&raw[binary.Size(unix.Cmsghdr{})]))
	if pktInfo.Addr != addr.Addr {
		t.Errorf("expected address %v, got %v", addr.Addr, pktInfo.Addr)
	}
	// reinterpret Ifindex as uint32 so this check compiles on every platform
	// regardless of the declared (int32/uint32) field type
	ifIndex := *(*uint32)(unsafe.Pointer(&pktInfo.Ifindex))
	if ifIndex != 7 {
		t.Errorf("expected Ifindex 7, got %d", ifIndex)
	}
}

func TestPktInfo6Cmsg(t *testing.T) {
	addr := unix.SockaddrInet6{Port: 320, Addr: [16]byte{0x20, 0x01, 0xc0, 0xff, 0xee, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2}}
	raw := pktInfo6Cmsg(&addr, 9)

	if len(raw) != unix.CmsgSpace(unix.SizeofInet6Pktinfo) {
		t.Errorf("unexpected control message size: %d != %d", len(raw), unix.CmsgSpace(unix.SizeofInet6Pktinfo))
	}

	h := (*unix.Cmsghdr)(unsafe.Pointer(&raw[0]))
	if h.Level != unix.IPPROTO_IPV6 {
		t.Errorf("expected level IPPROTO_IPV6, got %d", h.Level)
	}
	if h.Type != unix.IPV6_PKTINFO {
		t.Errorf("expected type IPV6_PKTINFO, got %d", h.Type)
	}

	pktInfo := (*unix.Inet6Pktinfo)(unsafe.Pointer(&raw[binary.Size(unix.Cmsghdr{})]))
	if pktInfo.Addr != addr.Addr {
		t.Errorf("expected address %v, got %v", addr.Addr, pktInfo.Addr)
	}
	if pktInfo.Ifindex != 9 {
		t.Errorf("expected Ifindex 9, got %d", pktInfo.Ifindex)
	}
}
