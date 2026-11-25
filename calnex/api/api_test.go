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

package api

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-ini/ini"
	"github.com/stretchr/testify/require"
)

func TestChannel(t *testing.T) {
	legitChannelNamesToChannel := map[string]Channel{
		"1":   ChannelONE,
		"2":   ChannelTWO,
		"C":   ChannelC,
		"D":   ChannelD,
		"VP1": ChannelVP1,
	}
	for channelS, channel := range legitChannelNamesToChannel {
		c, err := ChannelFromString(channelS)
		require.NoError(t, err)
		require.Equal(t, channel, *c)

		c = new(Channel)
		err = c.UnmarshalText([]byte(channelS))
		require.NoError(t, err)
		require.Equal(t, channel, *c)
	}

	wrongChannelNames := []string{"", "?", "z", "foo"}
	for _, channelS := range wrongChannelNames {
		c, err := ChannelFromString(channelS)
		require.Nil(t, c)
		require.ErrorIs(t, ErrBadChannel, err)

		c = new(Channel)
		err = c.UnmarshalText([]byte(channelS))
		require.ErrorIs(t, ErrBadChannel, err)
	}
}

func TestChannels(t *testing.T) {
	cs := Channels{}
	require.Equal(t, "channel", cs.Type())

	err := cs.Set("1")
	require.NoError(t, err)
	err = cs.Set("A")
	require.NoError(t, err)
	require.Equal(t, "1, A", cs.String())
}

func TestProbe(t *testing.T) {
	legitProbeNamesToProbe := map[string]Probe{
		"ntp": ProbeNTP,
		"ptp": ProbePTP,
		"pps": ProbePPS,
	}
	for probeS, probe := range legitProbeNamesToProbe {
		p, err := ProbeFromString(probeS)
		require.NoError(t, err)
		require.Equal(t, probe, *p)

		p = new(Probe)
		err = p.UnmarshalText([]byte(probeS))
		require.NoError(t, err)
		require.Equal(t, probe, *p)
	}
	wrongProbeNames := []string{"", "?", "z", "dns"}
	for _, probeS := range wrongProbeNames {
		p, err := ProbeFromString(probeS)
		require.Nil(t, p)
		require.ErrorIs(t, errBadProbe, err)

		p = new(Probe)
		err = p.UnmarshalText([]byte(probeS))
		require.ErrorIs(t, errBadProbe, err)
	}
}

func TestProbeFromCalnex(t *testing.T) {
	legitProbeNamesToProbe := map[string]Probe{
		"2":     ProbeNTP,
		"0":     ProbePTP,
		"1 PPS": ProbePPS,
	}
	for probeH, probe := range legitProbeNamesToProbe {
		p, err := ProbeFromCalnex(probeH)
		require.NoError(t, err)
		require.Equal(t, probe, *p)
	}
	wrongProbeNames := []string{"", "?", "z", "dns"}
	for _, probe := range wrongProbeNames {
		p, err := ProbeFromCalnex(probe)
		require.Nil(t, p)
		require.ErrorIs(t, errBadProbe, err)
	}
}

func TestCalnexName(t *testing.T) {
	require.Equal(t, "NTP", ProbeNTP.CalnexName())
	require.Equal(t, "PTP", ProbePTP.CalnexName())
	require.Equal(t, "1 PPS", ProbePPS.CalnexName())
}

func TestTLSSetting(t *testing.T) {
	calnexAPI, err := NewAPI("localhost", false, time.Second)
	require.NoError(t, err)
	// Never ever ever allow insecure over https
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}
	require.Equal(t, transport, calnexAPI.Client.Transport)

	calnexAPI, err = NewAPI("localhost", true, time.Second)
	require.NoError(t, err)
	transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	require.Equal(t, transport, calnexAPI.Client.Transport)
}

func TestFetchCsv(t *testing.T) {
	sampleResp := "1607961193.773740,-000.000000250501"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	legitChannelNames := []Channel{ChannelONE, ChannelTWO, ChannelC, ChannelD, ChannelVP22}

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()
	for _, channel := range legitChannelNames {
		lines, err := calnexAPI.FetchCsv(channel, true)
		require.NoError(t, err)
		require.Equal(t, 1, len(lines))
		require.Equal(t, sampleResp, strings.Join(lines[0], ","))
	}
}

func TestFetchCsvNoData(t *testing.T) {
	sampleResp := "{\"message\": \"No data available\", \"result\": true}"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()
	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()
	lines, err := calnexAPI.FetchCsv(ChannelVP22, true)
	require.Error(t, err)
	require.Nil(t, lines)
}

// newTestAPI returns an API pointed at a server answering every request with h
func newTestAPI(t *testing.T, h http.Handler) *API {
	t.Helper()

	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)

	parsed, err := url.Parse(ts.URL)
	require.NoError(t, err)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()
	return calnexAPI
}

// respondWith serves a fixed body
func respondWith(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, body)
	}
}

// respondStatus serves a status code and no body
func respondStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

func TestFetchMeasurements(t *testing.T) {
	sampleResp := `{
		"data": {
			"VP17": {
				"data": [],
				"info": {"channel": "VP17", "protocol": "PTP", "source": "calnex01", "target": "::1"}
			},
			"C": {
				"data": [{"timestamp": 1785935899.0, "value": -5.8164005167782307E-8}],
				"info": {"channel": "C", "protocol": "PPS", "source": "calnex01", "target": ""}
			},
			"keys": ["C", "VP17"]
		},
		"status": {"gnss": {"lockedSatellites": 12}}
	}`
	expected := &Measurements{
		Channels: []ChannelMeasurement{
			{
				Channel: ChannelC,
				Probe:   ProbePPS,
				Samples: []Sample{{Timestamp: 1785935899.0, Value: -5.8164005167782307e-8}},
			},
			{
				Channel: ChannelVP17,
				Probe:   ProbePTP,
				Samples: []Sample{},
			},
		},
	}

	m, err := newTestAPI(t, respondWith(sampleResp)).FetchMeasurements()
	require.NoError(t, err)
	require.Equal(t, expected, m)
}

// Firmware without the endpoint answers 501s
func TestFetchMeasurementsNotImplemented(t *testing.T) {
	m, err := newTestAPI(t, respondStatus(http.StatusNotImplemented)).FetchMeasurements()
	require.ErrorIs(t, err, ErrNotSupported)
	require.Nil(t, m)
}

// A code net/http has no text for must still be diagnosable
func TestFetchMeasurementsUnexpectedStatus(t *testing.T) {
	m, err := newTestAPI(t, respondStatus(599)).FetchMeasurements()
	require.ErrorContains(t, err, "599")
	require.Nil(t, m)
}

// An empty list is how a device says it is measuring nothing. It has to stay
// distinguishable from a response that omits the list altogether.
func TestFetchMeasurementsNoChannels(t *testing.T) {
	m, err := newTestAPI(t, respondWith(`{"data": {"keys": []}}`)).FetchMeasurements()
	require.NoError(t, err)
	require.Empty(t, m.Channels)
}

func TestFetchMeasurementsBadResponse(t *testing.T) {
	badResponses := map[string]struct{ resp, err string }{
		"not json":     {"certainly not json", "invalid character"},
		"no keys":      {`{"data": {}}`, "no channel list"},
		"null keys":    {`{"data": {"keys": null}}`, "null channel list"},
		"no data":      {`{}`, "no data section"},
		"bad keys":     {`{"data": {"keys": "C"}}`, "failed to parse channel list"},
		"bad channel":  {`{"data": {"keys": ["C"], "C": []}}`, "failed to parse channel C"},
		"null channel": {`{"data": {"keys": ["C"], "C": null}}`, "channel C has no data"},
		"listed channel with no entry": {
			`{"data": {"keys": ["C", "VP1"], "C": {"data": [], "info": {"protocol": "PPS"}}}}`,
			"channel VP1 is in the channel list with no entry",
		},
		"unlisted channel": {
			`{"data": {"keys": ["C"], "C": {"data": [], "info": {"protocol": "PPS"}},` +
				` "VP1": {"data": [], "info": {"protocol": "PTP"}}}}`,
			"channel VP1 has an entry but is not in the channel list",
		},
		"unlisted channel alias": {
			`{"data": {"keys": ["C"], "C": {"data": [], "info": {"protocol": "PPS"}},` +
				` "c": {"data": [], "info": {"protocol": "PPS"}}}}`,
			"channel c has an entry but is not in the channel list",
		},
		"missing channel data": {
			`{"data": {"keys": ["C"], "C": {"info": {"protocol": "PPS"}}}}`,
			"channel C has no data",
		},
		"bad data point": {
			`{"data": {"keys": ["C"], "C": {"data": [{"timestamp": "now", "value": 1}]}}}`,
			"failed to parse channel C",
		},
		"missing timestamp": {
			`{"data": {"keys": ["C"], "C": {"data": [{"value": 1}], "info": {"protocol": "PPS"}}}}`,
			"no timestamp",
		},
		"missing value": {
			`{"data": {"keys": ["C"], "C": {"data": [{"timestamp": 1}], "info": {"protocol": "PPS"}}}}`,
			"no value",
		},
		"mixed case channel": {
			`{"data": {"keys": ["C", "c"], "C": {"data": [], "info": {"protocol": "PPS"}},` +
				` "c": {"data": [], "info": {"protocol": "PPS"}}}}`,
			"channel C appears multiple times",
		},
		"unknown channel": {
			`{"data": {"keys": ["ZZ"], "ZZ": {"data": [], "info": {"protocol": "PPS"}}}}`,
			"unknown channel ZZ",
		},
		"unknown protocol": {
			`{"data": {"keys": ["C"], "C": {"data": [], "info": {"protocol": "CARRIER"}}}}`,
			`channel C has unknown protocol "CARRIER"`,
		},
		"missing info": {
			`{"data": {"keys": ["C"], "C": {"data": []}}}`,
			`channel C has unknown protocol ""`,
		},
	}

	for name, tt := range badResponses {
		t.Run(name, func(t *testing.T) {
			m, err := newTestAPI(t, respondWith(tt.resp)).FetchMeasurements()
			require.ErrorContains(t, err, tt.err)
			require.Nil(t, m)
		})
	}
}

func TestFetchChannelProtocol_NTP(t *testing.T) {
	sampleResp := "measure/ch9/ptp_synce/mode/probe_type=2"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	probe, err := calnexAPI.FetchChannelProbe(ChannelVP1)
	require.NoError(t, err)
	require.Equal(t, ProbeNTP, *probe)
}

func TestFetchChannelProtocol_PTP(t *testing.T) {
	sampleResp := "measure/ch10/ptp_synce/mode/probe_type=0"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	probe, err := calnexAPI.FetchChannelProbe(ChannelVP2)
	require.NoError(t, err)
	require.Equal(t, ProbePTP, *probe)
}

func TestFetchChannelProtocol_PPS(t *testing.T) {
	sampleResp := "measure/ch0/signal_type=1 PPS"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	probe, err := calnexAPI.FetchChannelProbe(ChannelA)
	require.NoError(t, err)
	require.Equal(t, ProbePPS, *probe)
}

func TestFetchChannelTarget_NTP(t *testing.T) {
	sampleResp := "measure/ch9/ptp_synce/ntp/server_ip_ipv6=fd00:3116:301a::3e"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	target, err := calnexAPI.FetchChannelTarget(ChannelVP1, ProbeNTP)
	require.NoError(t, err)
	require.Equal(t, "fd00:3116:301a::3e", target)
}

func TestFetchChannelTarget_PTP(t *testing.T) {
	sampleResp := "measure/ch9/ptp_synce/ptp/master_ip_ipv6=fd00:3116:301a::3e"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	target, err := calnexAPI.FetchChannelTarget(ChannelVP1, ProbePTP)
	require.NoError(t, err)
	require.Equal(t, "fd00:3116:301a::3e", target)
}

func TestFetchChannelTarget_PPS(t *testing.T) {
	sampleResp := "measure/ch0/signal_type=1 PPS"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	target, err := calnexAPI.FetchChannelTarget(ChannelA, ProbePPS)
	require.NoError(t, err)
	require.Equal(t, "1 PPS", target)
}

func TestFetchUsedChannels(t *testing.T) {
	sampleResp := "[measure]\nch0\\used=Yes\nch0\\installed=1\nch7\\used=No\nch7\\installed=1\nch29\\used=Yes\nch29\\installed=0\nch30\\used=Yes\nch30\\installed=1\n"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	expected := []Channel{ChannelA, ChannelVP22}
	used, err := calnexAPI.FetchUsedChannels()
	require.NoError(t, err)
	require.ElementsMatch(t, expected, used)
}

func TestFetchSettings(t *testing.T) {
	sampleResp := "[measure]\nch0\\synce_enabled=Off\n"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f, err := calnexAPI.FetchSettings()
	require.NoError(t, err)
	require.Equal(t, f.Section("measure").Key("ch0\\synce_enabled").Value(), OFF)
}

func TestFetchStatus(t *testing.T) {
	sampleResp := "{\"channelLinksReady\":true,\"ipAddressReady\":true,\"measurementActive\":true,\"measurementReady\":true,\"modulesReady\":true,\"referenceReady\":true}"
	expected := &Status{
		ChannelLinksReady: true,
		IPAddressReady:    true,
		MeasurementActive: true,
		MeasurementReady:  true,
		ModulesReady:      true,
		ReferenceReady:    true,
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f, err := calnexAPI.FetchStatus()
	require.NoError(t, err)
	require.Equal(t, expected, f)
}

func TestFetchInstumentStatus(t *testing.T) {
	sampleResp := "{\"Channels\":{\"1\":{\"Progress\":-1,\"Slot\":\"1\",\"State\":\"Ready\",\"Type\":\"10G Packet Module (V2)\"},\"2\":{\"Progress\":-1,\"Slot\":\"1\",\"State\":\"Ready\",\"Type\":\"10G Packet Module (V2)\"},\"C\":{\"Progress\":-1,\"Slot\":\"C\",\"State\":\"Ready\",\"Type\":\"Clock Module\"},\"D\":{\"Progress\":-1,\"Slot\":\"C\",\"State\":\"Ready\",\"Type\":\"Clock Module\"}},\"Modules\":{\"1\":{\"Channels\":[\"1\",\"2\"],\"Progress\":-1,\"State\":\"Ready\",\"Type\":\"Packet Module (V2)\"},\"C\":{\"Channels\":[\"C\",\"D\"],\"Progress\":-1,\"State\":\"Ready\",\"Type\":\"Clock Module\"}}}"
	cm := make(map[Channel]ChannelStatus)
	cm[ChannelONE] = ChannelStatus{
		Progress: -1,
		Slot:     "1",
		State:    "Ready",
		Type:     "10G Packet Module (V2)",
	}
	cm[ChannelTWO] = ChannelStatus{
		Progress: -1,
		Slot:     "1",
		State:    "Ready",
		Type:     "10G Packet Module (V2)",
	}
	cm[ChannelC] = ChannelStatus{
		Progress: -1,
		Slot:     "C",
		State:    "Ready",
		Type:     "Clock Module",
	}
	cm[ChannelD] = ChannelStatus{
		Progress: -1,
		Slot:     "C",
		State:    "Ready",
		Type:     "Clock Module",
	}

	mm := make(map[Channel]ModuleStatus)
	mm[ChannelONE] = ModuleStatus{
		Channels: []string{
			"1",
			"2",
		},
		Progress: -1,
		State:    "Ready",
		Type:     "Packet Module (V2)",
	}
	mm[ChannelC] = ModuleStatus{
		Channels: []string{
			"C",
			"D",
		},
		Progress: -1,
		State:    "Ready",
		Type:     "Clock Module",
	}
	expected := &InstrumentStatus{
		Channels: cm,
		Modules:  mm,
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f, err := calnexAPI.FetchInstrumentStatus()
	require.NoError(t, err)
	require.Equal(t, expected, f)
}

func TestPushSettings(t *testing.T) {
	sampleResp := "{\n\"result\": true\n}"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	sampleConfig := "[measure]\nch0\\synce_enabled=Off\n"
	f, err := ini.Load([]byte(sampleConfig))
	require.NoError(t, err)

	err = calnexAPI.PushSettings(f)
	require.NoError(t, err)
}

func TestFetchVersion(t *testing.T) {
	sampleResp := "{\"firmware\": \"2.13.1.0.5583D-20210924\"}"
	expected := &Version{
		Firmware: "2.13.1.0.5583D-20210924",
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f, err := calnexAPI.FetchVersion()
	require.NoError(t, err)
	require.Equal(t, expected, f)
}

func TestPushVersion(t *testing.T) {
	sampleResp := "{\n\"result\" : true,\n\"message\" : \"Installing firmware Version: 2.13.1.0.5583D-20210924\"\n}"
	expected := &Result{
		Result:  true,
		Message: "Installing firmware Version: 2.13.1.0.5583D-20210924",
	}
	// Firmware file itself
	fw, err := os.CreateTemp("/tmp", "calnex")
	require.NoError(t, err)
	defer fw.Close()
	defer os.Remove(fw.Name())
	_, err = fw.WriteString("Hello Calnex!")
	require.NoError(t, err)

	// Firmware file saved via http
	fwres, err := os.CreateTemp("/tmp", "calnex")
	require.NoError(t, err)
	defer os.Remove(fwres.Name())

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		defer r.Body.Close()
		defer fwres.Close()
		_, err := io.Copy(fwres, r.Body)
		require.NoError(t, err)

		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	r, err := calnexAPI.PushVersion(fw.Name())
	require.NoError(t, err)
	require.Equal(t, expected, r)

	originalFW, err := os.ReadFile(fw.Name())
	require.NoError(t, err)

	uploadedFW, err := os.ReadFile(fwres.Name())
	require.NoError(t, err)

	require.Equal(t, originalFW, uploadedFW)
}

func TestPost(t *testing.T) {
	sampleResp := "{\n\"result\" : true,\n\"message\" : \"LGTM\"\n}"
	expected := &Result{
		Result:  true,
		Message: "LGTM",
	}
	postData := []byte("Whatever")
	serverReceived := &bytes.Buffer{}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		defer r.Body.Close()
		_, err := serverReceived.ReadFrom(r.Body)
		require.NoError(t, err)
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	buf := bytes.NewBuffer(postData)
	r, err := calnexAPI.post(parsed.String(), buf)
	require.NoError(t, err)
	require.Equal(t, expected, r)
	require.Equal(t, postData, serverReceived.Bytes())
}

func TestGet(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if strings.Contains(r.URL.Path, "getstatus") {
			// FetchStatus
			fmt.Fprintln(w, "{\n\"referenceReady\": true,\n\"modulesReady\": true,\n\"measurementActive\": true\n}")
		} else {
			fmt.Fprintln(w, "{\n\"result\": true\n}")
		}
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	err = calnexAPI.StartMeasure()
	require.NoError(t, err)

	err = calnexAPI.StopMeasure()
	require.NoError(t, err)

	err = calnexAPI.ClearDevice()
	require.NoError(t, err)

	err = calnexAPI.Reboot(false)
	require.NoError(t, err)

	err = calnexAPI.Reboot(true)
	require.NoError(t, err)
}

func TestHTTPError(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f := ini.Empty()
	err = calnexAPI.PushSettings(f)
	require.Error(t, err)
}

func TestFetchProblemReport(t *testing.T) {
	expectedReportContent := "I am a problem report"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprint(w, expectedReportContent)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	dir, err := os.MkdirTemp("/tmp", "calnex")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	reportFilePath, err := calnexAPI.FetchProblemReport(dir)
	require.NoError(t, err)
	require.FileExists(t, reportFilePath)
	defer os.Remove(reportFilePath)

	require.Contains(t, reportFilePath, "calnex_problem_report_")
	require.Contains(t, reportFilePath, ".tar")

	reportContent, err := os.ReadFile(reportFilePath)
	require.NoError(t, err)

	require.Equal(t, expectedReportContent, string(reportContent))
}

func TestPushCert(t *testing.T) {
	sampleResp := "{\n\"result\" : true,\n\"message\" : \"The API Interface will now be restarted\"\n}"
	expected := &Result{
		Result:  true,
		Message: "The API Interface will now be restarted",
	}
	// @lint-ignore PRIVATEKEY insecure-private-key-storage
	cert := []byte("-----BEGIN CERTIFICATE-----\nI am a certificate\n-----END CERTIFICATE-----\n-----BEGIN RSA PRIVATE KEY-----I am a key-----END RSA PRIVATE KEY-----")

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, cert, body)
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	r, err := calnexAPI.PushCert(cert)
	require.NoError(t, err)
	require.Equal(t, expected, r)
}

func TestParseResponse(t *testing.T) {
	expected := "500 mV"
	r, err := parseResponse("ch0\\trig_level=500 mV")
	require.NoError(t, err)
	require.Equal(t, expected, r)

	r, err = parseResponse("invalid")
	require.Equal(t, "", r)
	require.ErrorIs(t, errAPI, err)

	r, err = parseResponse("too=many=parts")
	require.Equal(t, "", r)
	require.ErrorIs(t, errAPI, err)
}

func TestMeasureChannelDatatypeMap(t *testing.T) {
	for i := 0; i <= 5; i++ {
		ch, err := ChannelFromInt(i)
		require.NoError(t, err)
		require.Equal(t, TE, MeasureChannelDatatypeMap[*ch])
	}

	for i := 6; i <= 8; i++ {
		ch, err := ChannelFromInt(i)
		require.NoError(t, err)
		_, ok := MeasureChannelDatatypeMap[*ch]
		require.False(t, ok)
	}

	for i := 9; i <= 40; i++ {
		ch, err := ChannelFromInt(i)
		require.NoError(t, err)
		require.Equal(t, TWOWAYTE, MeasureChannelDatatypeMap[*ch])
	}
}

func TestGnssStatus(t *testing.T) {
	sampleResp := "{\"antennaStatus\":\"OK\",\"locked\":true,\"lockedSatellites\":9,\"surveyComplete\":true,\"surveyPercentComplete\":100}"
	expected := &GNSS{
		AntennaStatus:         "OK",
		Locked:                true,
		LockedSatellites:      9,
		SurveyComplete:        true,
		SurveyPercentComplete: 100,
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	g, err := calnexAPI.GnssStatus()
	require.NoError(t, err)
	require.Equal(t, expected, g)
}

func TestPushLicense(t *testing.T) {
	sampleResp := "{\n\"result\" : true}"
	expected := &Result{
		Result:  true,
		Message: "",
	}
	// License file itself
	license, err := os.CreateTemp("/tmp", "calnex")
	require.NoError(t, err, "Failed to create temp license file.")
	defer license.Close()
	defer os.Remove(license.Name())
	_, err = license.WriteString("#SENTINEL license file")
	require.NoError(t, err, "Failed to write into temp license file.")

	// License file saved via http
	licenseRes, err := os.CreateTemp("/tmp", "calnexls")
	require.NoError(t, err, "Failed to write temp license file for HTTP")
	defer os.Remove(licenseRes.Name())

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		defer r.Body.Close()
		defer licenseRes.Close()
		_, err := io.Copy(licenseRes, r.Body)
		require.NoError(t, err)

		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	r, err := calnexAPI.PushLicense(license.Name())
	require.NoError(t, err, "Error occurred while uploading license file.")
	require.Equal(t, expected, r)

	originalLicense, err := os.ReadFile(license.Name())
	require.NoError(t, err)

	uploadedLicense, err := os.ReadFile(licenseRes.Name())
	require.NoError(t, err)

	require.Equal(t, originalLicense, uploadedLicense)
}

func TestPushLicenseError(t *testing.T) {
	sampleResp := "{\"message\":\"License file data is not valid.\",\"result\":false}"
	var expected *Result

	// License file itself
	license, err := os.CreateTemp("/tmp", "calnex")
	require.NoError(t, err, "Failed to create temp license file.")
	defer license.Close()
	defer os.Remove(license.Name())
	_, err = license.WriteString("#SENTINEL license file")
	require.NoError(t, err, "Failed to write into temp license file.")

	// License file saved via http
	licenseRes, err := os.CreateTemp("/tmp", "calnexls")
	require.NoError(t, err, "Failed to write temp license file for HTTP")
	defer os.Remove(licenseRes.Name())

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		defer r.Body.Close()
		defer licenseRes.Close()
		_, err := io.Copy(licenseRes, r.Body)
		require.NoError(t, err)

		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	r, err := calnexAPI.PushLicense(license.Name())
	require.Equal(t, expected, r)
	require.Error(t, err, "Invalid license file should yield an error.")

	originalLicense, err := os.ReadFile(license.Name())
	require.NoError(t, err)

	uploadedLicense, err := os.ReadFile(licenseRes.Name())
	require.NoError(t, err)

	require.Equal(t, originalLicense, uploadedLicense)
}

func TestPowerSupplyStatus(t *testing.T) {
	sampleResp := "{\"power_supply_good\":false,\"supplies\":[{\"comms_good\":true,\"name\":\"PSU_module_A\",\"status_good\":true},{\"comms_good\":true,\"name\":\"PSU_module_B\",\"status_good\":false}]}"
	expected := &PowerSupplyStatus{
		PowerSupplyGood: false,
		Supplies: []PowerSupply{
			{
				CommsGood:  true,
				Name:       "PSU_module_A",
				StatusGood: true,
			},
			{
				CommsGood:  true,
				Name:       "PSU_module_B",
				StatusGood: false,
			},
		},
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	g, err := calnexAPI.PowerSupplyStatus()
	require.NoError(t, err)
	require.Equal(t, expected, g)
}

func TestPowerSupplyStatusSentinel(t *testing.T) {
	sampleResp := "{\"power_supply_good\":false}"
	expected := &PowerSupplyStatus{
		PowerSupplyGood: false,
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	g, err := calnexAPI.PowerSupplyStatus()
	require.NoError(t, err)
	require.Equal(t, expected, g)
}

func TestFetchUptime(t *testing.T) {
	sampleResp := "{\"uptime\": 42}"
	expected := &Uptime{
		Uptime: 42,
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f, err := calnexAPI.FetchUptime()
	require.NoError(t, err)
	require.Equal(t, expected, f)
}

func TestEnableSFTP(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sftp/enable" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, `{"result": true}`)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	require.NoError(t, calnexAPI.EnableSFTP())
}

func TestFetchSystemMonitor(t *testing.T) {
	sampleResp := `{"cpu_usage":48.01,"free_memory":380489728,"installed_memory":1034510336,"memory_usage":540426240,"start_time":3940,"system_time":5484133,"thread_id":2660,"threads":24,"user_time":180330942}`
	expected := &SystemMonitor{
		CPUUsage:        48.01,
		FreeMemory:      380489728,
		InstalledMemory: 1034510336,
		MemoryUsage:     540426240,
		Threads:         24,
	}

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, sampleResp)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()

	f, err := calnexAPI.FetchSystemMonitor()
	require.NoError(t, err)
	require.Equal(t, expected, f)
}

func TestFormatHost(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "IPv6 full", input: "2401:db00:2715:f002:face:0000:021a:0000", want: "[2401:db00:2715:f002:face:0000:021a:0000]"},
		{name: "IPv6 loopback short", input: "::1", want: "[::1]"},
		{name: "IPv6 already bracketed", input: "[2401:db00:2715:f002:face:0000:021a:0000]", want: "[2401:db00:2715:f002:face:0000:021a:0000]"},
		{name: "IPv4 plain", input: "10.128.64.5", want: "10.128.64.5"},
		{name: "FQDN", input: "calnex01.dc1.facebook.com", want: "calnex01.dc1.facebook.com"},
		{name: "localhost", input: "localhost", want: "localhost"},

		{name: "empty", input: "", wantErr: true},
		{name: "spaces", input: "not valid", wantErr: true},
		{name: "garbage in brackets", input: "[garbage]", wantErr: true},
		{name: "IPv4 in brackets", input: "[10.128.64.5]", wantErr: true},
		{name: "path injection", input: "host/extra", wantErr: true},
		{name: "userinfo injection", input: "user@host", wantErr: true},
		{name: "query injection", input: "host?x=1", wantErr: true},
		{name: "fragment injection", input: "host#frag", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := formatHost(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
