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

package client

import (
	"testing"

	"github.com/facebook/time/timestamp"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// ASAN forces unsafe.Pointer conversions onto the heap, so these only build without it

// TestReadPacketBufZeroAlloc keeps the per-packet receive of the sptp listener allocation free
func TestReadPacketBufZeroAlloc(t *testing.T) {
	conn, clientFd, local := stampedConnPair(t)
	buf := make([]byte, timestamp.PayloadSizeBytes)
	oob := make([]byte, timestamp.ControlSizeBytes)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		require.NoError(t, unix.Sendto(clientFd, buf[:44], 0, local))
		_, _, _, err := conn.ReadPacketBuf(buf, oob)
		require.NoError(t, err)
	}))
}
