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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	// Registers the profiling endpoints on http.DefaultServeMux, exactly as the
	// sptp-exporter binary's own blank import does. Without it the process the
	// test runs in would not have the exposure the test is looking for.
	_ "net/http/pprof" //nolint:gosec // G108 is the bug under test, reproduced on purpose

	"github.com/stretchr/testify/require"
)

func TestPrometheusExporterDoesNotServePprof(t *testing.T) {
	_, pprofPattern := http.DefaultServeMux.Handler(
		httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil),
	)
	require.NotEmpty(t, pprofPattern, "net/http/pprof is no longer linked, this test proves nothing")

	// Start scrapes sptp on [::1] once and exits the process if that fails.
	sptpListener, err := net.Listen("tcp", "[::1]:0")
	require.NoError(t, err)
	sptp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"ptp.sptp.portstats.rx.sync":4656}`)
	}))
	require.NoError(t, sptp.Listener.Close())
	sptp.Listener = sptpListener
	sptp.Start()
	defer sptp.Close()

	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	e := NewPrometheusExporter(port, sptpListener.Addr().(*net.TCPAddr).Port, time.Second)
	go e.Start()
	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/metrics")
		if err != nil {
			return false
		}
		body, err := io.ReadAll(resp.Body)
		if resp.Body.Close() != nil || err != nil {
			return false
		}
		return resp.StatusCode == http.StatusOK && strings.Contains(string(body), "ptp_sptp_portstats_rx_sync 4656")
	}, 30*time.Second, 10*time.Millisecond, "exporter never served the scraped metrics on %s", base)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol"} {
		resp, err := http.Get(base + path)
		require.NoError(t, err, path)
		require.NoError(t, resp.Body.Close(), path)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
}

// freePort returns a port nothing is listening on, for Start to bind.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}
