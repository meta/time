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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

// PrometheusExporter holds the exporter details
type PrometheusExporter struct {
	registry   *prometheus.Registry
	listenPort int
	sptpPort   int
	interval   time.Duration
}

// NewPrometheusExporter creates a new instance of PrometheusExporter
func NewPrometheusExporter(listenPort int, sptpPort int, scrapeInterval time.Duration) *PrometheusExporter {
	return &PrometheusExporter{registry: prometheus.NewRegistry(), interval: scrapeInterval, listenPort: listenPort, sptpPort: sptpPort}
}

// Start starts the exporter. The mux must not be http.DefaultServeMux: the
// sptp-exporter binary links net/http/pprof, whose init registers the profiling
// endpoints there, and this port listens on every interface.
func (e *PrometheusExporter) Start() {
	go func() {
		e.scrapeMetrics()

		time.Sleep(e.interval)
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		e.registry,
		promhttp.HandlerOpts{
			// Opt into OpenMetrics to support exemplars.
			EnableOpenMetrics: true,
		},
	))

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", e.listenPort), mux))
}

func (e *PrometheusExporter) scrapeMetrics() {
	counters, err := FetchCounters(fmt.Sprintf("http://[::1]:%d", e.sptpPort))
	if err != nil {
		log.Fatalf("Failed to fetch sptp metrics :%v", err)
	}
	for mkey, mval := range counters {
		promCollector := prometheus.NewGauge(prometheus.GaugeOpts{
			Name: flattenKey(mkey),
			Help: mkey,
		})
		if err := e.registry.Register(promCollector); err != nil {
			are := &prometheus.AlreadyRegisteredError{}
			if errors.As(err, are) {
				promCollector = are.ExistingCollector.(prometheus.Gauge)
			} else {
				log.Errorf("failed to register metric %s %v", mkey, err)
				continue
			}
		}
		promCollector.Set(float64(mval))
	}
}

func flattenKey(key string) string {
	key = strings.ReplaceAll(key, " ", "_")
	key = strings.ReplaceAll(key, ".", "_")
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, "=", "_")
	key = strings.ReplaceAll(key, "/", "_")
	return key
}
