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

package cmd

import (
	"bytes"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/facebook/time/fbclock"

	"github.com/stretchr/testify/require"
)

// The fbagent collectors' grep, verbatim from ptp/ptp_fbclock.sh and
// ntp/ntp_fbclock.sh in opsfiles/.../fb_fbagent/files/default/collectors/.
var collectorFilter = regexp.MustCompile(`(earliest_ns|latest_ns|requests)`)

var fbclockBinSample = &fbclock.TrueTime{
	Earliest: time.Unix(0, 1790343749019905602),
	Latest:   time.Unix(0, 1790343749019907266),
}

// Every key one run can print, built the way fbclockRun builds it.
func stdoutKeys(t *testing.T, sources fbclock.Sources) []string {
	t.Helper()
	prefix, msBuckets := fbclockKeys(sources)
	out := map[string]int64{}
	fbclockSample(&fbclock.TrueTime{
		Earliest: time.Unix(0, 1648137249050666302),
		Latest:   time.Unix(0, 1648137249050666313),
	}, prefix, out)
	fbclockMetrics(fbclock.Stats{Requests: 10000, Errors: 639}, prefix, ".60", msBuckets, out)
	return slices.Sorted(maps.Keys(out))
}

// Each key is one ODS timeseries on every PTP client host, so adding one is a
// fleet-wide decision. Pinned so it happens in review.
func TestFbclockMetricKeys(t *testing.T) {
	require.Equal(t, []string{
		"ptp.fbclock_synthetic.api.earliest_ns",
		"ptp.fbclock_synthetic.api.errors.sum.60",
		"ptp.fbclock_synthetic.api.latest_ns",
		"ptp.fbclock_synthetic.api.requests.sum.60",
		"ptp.fbclock_synthetic.api.wou_ge_1000us.sum.60",
		"ptp.fbclock_synthetic.api.wou_lt_1000us.sum.60",
		"ptp.fbclock_synthetic.api.wou_lt_100us.sum.60",
		"ptp.fbclock_synthetic.api.wou_lt_10us.sum.60",
		"ptp.fbclock_synthetic.api.wou_ns",
		"ptp.fbclock_synthetic.api.wou_ns.avg.60",
		"ptp.fbclock_synthetic.api.wou_ns.max.60",
	}, stdoutKeys(t, fbclock.SourcePTP))
}

// Only an ntp-only run gets these, the NTP synthetic's keys; any keeps PTP's.
func TestFbclockNTPMetricKeys(t *testing.T) {
	require.Equal(t, []string{
		"ntp.fbclock_synthetic.api.earliest_ns",
		"ntp.fbclock_synthetic.api.errors.sum.60",
		"ntp.fbclock_synthetic.api.latest_ns",
		"ntp.fbclock_synthetic.api.requests.sum.60",
		"ntp.fbclock_synthetic.api.wou_ge_100ms.sum.60",
		"ntp.fbclock_synthetic.api.wou_lt_100ms.sum.60",
		"ntp.fbclock_synthetic.api.wou_lt_10ms.sum.60",
		"ntp.fbclock_synthetic.api.wou_lt_1ms.sum.60",
		"ntp.fbclock_synthetic.api.wou_ns",
		"ntp.fbclock_synthetic.api.wou_ns.avg.60",
		"ntp.fbclock_synthetic.api.wou_ns.max.60",
	}, stdoutKeys(t, fbclock.SourceNTP))
	require.Equal(t, stdoutKeys(t, fbclock.SourcePTP), stdoutKeys(t, fbclock.SourceAny))
}

// Both collectors grep whole lines out of a one-key-per-line JSON object. If the last key were
// one they drop, the line before keeps its trailing comma and fbagent discards the host's run.
func TestFbclockStdoutSurvivesCollectorFilter(t *testing.T) {
	for _, sources := range []fbclock.Sources{fbclock.SourcePTP, fbclock.SourceNTP} {
		keys := stdoutKeys(t, sources)
		require.NotEmpty(t, keys)
		require.NotRegexp(t, collectorFilter, keys[len(keys)-1])

		// That rests on json.Marshal sorting keys, so read the order off the wire.
		out := map[string]int64{}
		for i, k := range keys {
			out[k] = int64(i)
		}
		raw, err := json.Marshal(out)
		require.NoError(t, err)

		dec := json.NewDecoder(bytes.NewReader(raw))
		_, err = dec.Token()
		require.NoError(t, err)
		var order []string
		for dec.More() {
			name, err := dec.Token()
			require.NoError(t, err)
			order = append(order, name.(string))
			var v int64
			require.NoError(t, dec.Decode(&v))
		}
		require.Equal(t, keys, order)
	}
}

// The fbagent collector passes -j.
func TestFbclockTextByDefault(t *testing.T) {
	f := fbclockCmd.Flags().ShorthandLookup("j")
	require.NotNil(t, f)
	require.Equal(t, "json", f.Name)
	require.Equal(t, "false", f.DefValue)
}

// Defaults to ptp, so the fbagent collector, which doesn't pass it, keeps
// measuring PTP only.
func TestFbclockSourcesDefaultToPTP(t *testing.T) {
	f := fbclockCmd.Flags().Lookup("sources")
	require.NotNil(t, f)
	require.Equal(t, "ptp", f.DefValue)
}

func TestFbclockV3DefaultsOff(t *testing.T) {
	f := fbclockCmd.Flags().Lookup("v3")
	require.NotNil(t, f)
	require.Equal(t, "false", f.DefValue)
}

func TestFbclockPrintText(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, fbclockPrintText(&buf, fbclockBinSample))
	require.Equal(t, "TrueTime:\n\tEarliest: 1790343749019905602\n\tLatest: 1790343749019907266\n\tWOU=1664 ns\n", buf.String())
}

func TestFbclockPrintJSON(t *testing.T) {
	var buf bytes.Buffer
	s := fbclock.Stats{Requests: 1, WOUAvg: 1664, WOUMax: 1664, WOUlt10us: 1}
	require.NoError(t, fbclockPrintJSON(&buf, fbclockBinSample, s, "p.", ".1", false))
	got := map[string]int64{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, map[string]int64{
		"p.earliest_ns":         1790343749019905602,
		"p.latest_ns":           1790343749019907266,
		"p.wou_ns":              1664,
		"p.wou_ns.avg.1":        1664,
		"p.wou_ns.max.1":        1664,
		"p.wou_lt_10us.sum.1":   1,
		"p.wou_lt_100us.sum.1":  0,
		"p.wou_lt_1000us.sum.1": 0,
		"p.wou_ge_1000us.sum.1": 0,
		"p.errors.sum.1":        0,
		"p.requests.sum.1":      1,
	}, got)
}

// A run where every call failed still emits JSON, so the collector records errors.sum.
func TestFbclockPrintJSONNoSample(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, fbclockPrintJSON(&buf, nil, fbclock.Stats{Requests: 1, Errors: 1}, "p.", ".1", false))
	got := map[string]int64{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, int64(1), got["p.errors.sum.1"])
	require.NotContains(t, got, "p.wou_ns")
	require.NotContains(t, got, "p.earliest_ns")
}

func TestFormatErrorCauses(t *testing.T) {
	require.Empty(t, formatErrorCauses(nil))
	require.Empty(t, formatErrorCauses(map[string]int64{}))

	// Ordered so the same fleet condition prints byte-identically twice.
	got := formatErrorCauses(map[string]int64{
		"WOU is too big":                            7,
		"no data from daemon error":                 639,
		"shared memory data too old to extrapolate": 7,
	})
	require.Equal(t, "  639\tno data from daemon error\n  7\tWOU is too big\n  7\tshared memory data too old to extrapolate\n", got)
}
