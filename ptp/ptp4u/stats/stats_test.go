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

package stats

import (
	"slices"
	"testing"

	ptp "github.com/facebook/time/ptp/protocol"
	"github.com/stretchr/testify/require"
)

func TestSyncMapInt64Keys(t *testing.T) {
	s := syncMapInt64{}
	s.init()

	expected := []int{24, 42}
	for _, i := range expected {
		s.inc(i)
	}

	found := 0
	for _, k := range s.keys() {
		if slices.Contains(expected, k) {
			found++
		}
	}
	require.Equal(t, len(expected), found)
}

func TestSyncMapInt64Copy(t *testing.T) {
	s := syncMapInt64{}
	s.init()

	s.store(1, 1)
	require.Equal(t, int64(1), s.load(1))

	dst := syncMapInt64{}
	dst.init()

	s.copy(&dst)
	require.Equal(t, s.m, dst.m)
	require.Equal(t, int64(1), dst.load(1))
}

func TestSyncMapInt64Counters(t *testing.T) {
	c := counters{}
	c.init(1)

	c.subscriptions.store(1, 1)
	c.rx.store(1, 1)
	c.tx.store(1, 1)
	c.rxSignalingGrant.store(1, 1)
	c.txSignalingCancel.store(1, 1)
	c.workerQueue.store(1, 1)
	c.workerSubs.store(1, 1)
	c.txtsattempts.store(1, 1)
	c.utcoffsetSec = 1
	c.clockaccuracy = 1
	c.clockclass = 1
	c.drain = 1
	c.reload = 1
	c.txtsMissing = 5
	c.minMaxCF = 6969
	c.rxDrops.store(0, 11)

	require.Equal(t, int64(1), c.subscriptions.load(1))
	require.Equal(t, int64(1), c.rx.load(1))
	require.Equal(t, int64(1), c.tx.load(1))
	require.Equal(t, int64(1), c.rxSignalingGrant.load(1))
	require.Equal(t, int64(1), c.txSignalingCancel.load(1))
	require.Equal(t, int64(1), c.workerQueue.load(1))
	require.Equal(t, int64(1), c.workerSubs.load(1))
	require.Equal(t, int64(1), c.txtsattempts.load(1))
	require.Equal(t, int64(1), c.utcoffsetSec)
	require.Equal(t, int64(1), c.clockaccuracy)
	require.Equal(t, int64(1), c.clockclass)
	require.Equal(t, int64(1), c.drain)
	require.Equal(t, int64(1), c.reload)
	require.Equal(t, int64(5), c.txtsMissing)
	require.Equal(t, int64(6969), c.minMaxCF)
	require.Equal(t, int64(11), c.rxDrops.load(0))

	c.reset()

	require.Equal(t, int64(0), c.subscriptions.load(1))
	require.Equal(t, int64(0), c.rx.load(1))
	require.Equal(t, int64(0), c.tx.load(1))
	require.Equal(t, int64(0), c.rxSignalingGrant.load(1))
	require.Equal(t, int64(0), c.txSignalingCancel.load(1))
	require.Equal(t, int64(0), c.workerQueue.load(1))
	require.Equal(t, int64(0), c.workerSubs.load(1))
	require.Equal(t, int64(0), c.txtsattempts.load(1))
	require.Equal(t, int64(0), c.utcoffsetSec)
	require.Equal(t, int64(0), c.clockaccuracy)
	require.Equal(t, int64(0), c.clockclass)
	require.Equal(t, int64(0), c.drain)
	require.Equal(t, int64(0), c.reload)
	require.Equal(t, int64(0), c.txtsMissing)
	require.Equal(t, int64(0), c.minMaxCF)
	require.Equal(t, int64(0), c.rxDrops.load(0))
}

func TestCountersToMap(t *testing.T) {
	c := counters{}
	c.init(1)

	c.subscriptions.store(int(ptp.MessageAnnounce), 1)
	c.tx.store(int(ptp.MessageSync), 2)
	c.rxSignalingGrant.store(int(ptp.MessageDelayResp), 3)
	c.rxSignalingCancel.store(int(ptp.MessageSync), 1)
	c.utcoffsetSec = 1
	c.clockaccuracy = 42
	c.clockclass = 6
	c.drain = 1
	c.reload = 2
	c.txtsMissing = 5
	c.minMaxCF = 6969
	c.rxDrops.store(0, 69)

	result := c.toMap()

	expectedMap := make(map[string]int64)
	expectedMap["subscriptions.announce"] = 1
	expectedMap["tx.sync"] = 2
	expectedMap["rx.signaling.grant.delay_resp"] = 3
	expectedMap["rx.signaling.cancel.sync"] = 1
	expectedMap["utcoffset_sec"] = 1
	expectedMap["clockaccuracy"] = 42
	expectedMap["clockclass"] = 6
	expectedMap["drain"] = 1
	expectedMap["reload"] = 2
	expectedMap["txts.missing"] = 5
	expectedMap["min_max_cf"] = 6969
	expectedMap["rxdrops"] = 69

	require.Equal(t, expectedMap, result)
}
