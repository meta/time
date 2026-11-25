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
	"io"
	"net"
	"os"
	"testing"
	"time"

	ntp "github.com/facebook/time/ntp/protocol"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestRefIDIPv4(t *testing.T) {
	// refID prints to stdout; just check it doesn't error on valid IPv4
	err := refID("192.168.1.1")
	require.NoError(t, err)
}

func TestRefIDIPv6(t *testing.T) {
	err := refID("2001:db8::1")
	require.NoError(t, err)
}

func TestRefIDEmpty(t *testing.T) {
	err := refID("")
	require.Error(t, err)
}

func TestRefIDInvalid(t *testing.T) {
	err := refID("not-an-ip")
	require.Error(t, err)
}

func TestStripZeroes(t *testing.T) {
	testCases := []struct {
		input float64
		want  string
	}{
		{input: 1.50, want: "1.5"},
		{input: 2.00, want: "2"},
		{input: 3.14, want: "3.14"},
		{input: 0.10, want: "0.1"},
		{input: 0.00, want: "0"},
		{input: 100.00, want: "100"},
		{input: 42.01, want: "42.01"},
		{input: -1.50, want: "-1.5"},
		{input: -0.10, want: "-0.1"},
	}
	for _, tc := range testCases {
		t.Run(tc.want, func(t *testing.T) {
			got := stripZeroes(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestConfigureVerbosity(t *testing.T) {
	verbose = false
	ConfigureVerbosity()
	require.Equal(t, log.InfoLevel, log.GetLevel())

	verbose = true
	ConfigureVerbosity()
	require.Equal(t, log.DebugLevel, log.GetLevel())
}

// startNTPServer runs an NTP server on localhost that is one second ahead.
// It answers the requests for which bad returns true with a truncated packet.
func startNTPServer(t *testing.T, bad func(n int) bool) string {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1024)
		for n := 0; ; n++ {
			l, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if bad(n) {
				_, _ = conn.WriteTo([]byte{0}, addr)
				continue
			}
			req, err := ntp.BytesToPacket(buf[:l])
			if err != nil {
				return
			}
			sec, frac := ntp.Time(time.Now().Add(time.Second))
			resp := &ntp.Packet{
				Settings:     0x24,
				Stratum:      1,
				OrigTimeSec:  req.TxTimeSec,
				OrigTimeFrac: req.TxTimeFrac,
				RxTimeSec:    sec,
				RxTimeFrac:   frac,
				TxTimeSec:    sec,
				TxTimeFrac:   frac,
			}
			b, err := resp.Bytes()
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(b, addr)
		}
	}()
	_, port, err := net.SplitHostPort(conn.LocalAddr().String())
	require.NoError(t, err)
	return port
}

func TestNtpDateAverageSkipsFailedRequests(t *testing.T) {
	port := startNTPServer(t, func(n int) bool { return n == 0 })

	r, w, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = w
	err = ntpDate("127.0.0.1", port, 4, "")
	w.Close()
	os.Stdout = stdout
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)

	// the server is 1s ahead, so the average offset must be close to 1s
	require.Contains(t, string(out), "Average (3 requests):\nOffset: 1.0")
}

func TestNtpDateNoValidResponses(t *testing.T) {
	port := startNTPServer(t, func(int) bool { return true })
	require.Error(t, ntpDate("127.0.0.1", port, 2, ""))
}
