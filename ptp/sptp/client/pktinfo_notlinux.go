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

package client

import "golang.org/x/sys/unix"

// setInet4PktinfoIfindex stores the interface index in an IPv4 pktinfo
// control message. Non-Linux platforms that have Inet4Pktinfo (darwin,
// solaris, z/OS) declare Ifindex as uint32.
func setInet4PktinfoIfindex(pktInfo *unix.Inet4Pktinfo, ifIndex int32) {
	pktInfo.Ifindex = uint32(ifIndex)
}
