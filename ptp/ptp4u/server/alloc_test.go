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

package server

import (
	"testing"

	"github.com/facebook/time/timestamp"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// ASAN forces unsafe.Pointer conversions onto the heap, so these only build without it

// TestHandleEventMessagesDelayReqZeroAlloc keeps receiving a DELAY_REQ from a known client allocation free
func TestHandleEventMessagesDelayReqZeroAlloc(t *testing.T) {
	clientFD, req, serverAddr, queue := eventServerDelayReq(t)
	// AllocsPerRun counts the whole process, so the receiver goroutine is measured too
	require.Zero(t, testing.AllocsPerRun(100, func() {
		require.NoError(t, unix.Sendto(clientFD, req, 0, serverAddr))
		<-queue
	}))
}

// TestSendWorkerDelayReqZeroAlloc keeps answering a DELAY_REQ (sync, TX timestamp, announce) allocation free
func TestSendWorkerDelayReqZeroAlloc(t *testing.T) {
	w, sc, clientFD := sendWorkerDelayReq(t)
	buf := make([]byte, timestamp.PayloadSizeBytes)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		w.queue <- sc
		_, err := unix.Read(clientFD, buf) // sync
		require.NoError(t, err)
		_, err = unix.Read(clientFD, buf) // announce
		require.NoError(t, err)
	}))
}
