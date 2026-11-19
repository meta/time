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

package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/facebook/time/ptp/ptp4u/drain"
	"github.com/facebook/time/ptp/ptp4u/server"
	"github.com/facebook/time/ptp/ptp4u/stats"
	"github.com/facebook/time/timestamp"
	log "github.com/sirupsen/logrus"
)

func main() {
	// Set reasonable defaults for Dynamic config
	c := &server.Config{
		DynamicConfig: server.DynamicConfig{
			ClockAccuracy:  0x21,
			ClockClass:     6,
			DrainInterval:  30 * time.Second,
			MaxSubDuration: 1 * time.Hour,
			MetricInterval: 1 * time.Minute,
			MinSubInterval: 1 * time.Second,
			UTCOffset:      37 * time.Second,
		},
	}

	var ipaddr string

	flag.IntVar(&c.DSCP, "dscp", 0, "DSCP for PTP packets, valid values are between 0-63 (used by send workers)")
	flag.IntVar(&c.MonitoringPort, "monitoringport", 8888, "Port to run monitoring server on")
	flag.IntVar(&c.QueueSize, "queue", 0, "Size of the queue to send out packets")
	flag.IntVar(&c.RecvWorkers, "recvworkers", 10, "Set the number of receive workers")
	flag.IntVar(&c.AttemptsTXTS, "txtsattempts", 50, "Max attempts to read a TX timestamp, 1ms apart")
	flag.IntVar(&c.SendWorkers, "workers", 100, "Set the number of send workers")
	flag.UintVar(&c.DomainNumber, "domainnumber", 0, "Set the PTP domain by its number. Valid values are [0-255]")
	flag.StringVar(&c.ConfigFile, "config", "", "Path to a config with dynamic settings")
	flag.StringVar(&c.DebugAddr, "pprofaddr", "", "host:port for the pprof to bind")
	flag.StringVar(&c.Interface, "iface", "eth0", "Set the interface")
	flag.StringVar(&c.LogLevel, "loglevel", "warning", "Set a log level. Can be: debug, info, warning, error")
	flag.StringVar(&c.PidFile, "pidfile", "/var/run/ptp4u.pid", "Pid file location")
	flag.TextVar(&c.TimestampType, "timestamptype", timestamp.HW, fmt.Sprintf("Timestamp type. Can be: %s, %s", timestamp.HW, timestamp.SW))
	flag.StringVar(&ipaddr, "ip", "::", "IP to bind on")
	flag.StringVar(&c.DrainFileName, "drainfile", "/var/tmp/kill_ptp4u", "ptp4u drain file location")
	flag.StringVar(&c.UndrainFileName, "undrainfile", "/var/tmp/unkill_ptp4u", "ptp4u force undrain file location")
	flag.Parse()

	switch c.LogLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.Fatalf("Unrecognized log level: %v", c.LogLevel)
	}

	if c.ConfigFile != "" {
		dc, err := server.ReadDynamicConfig(c.ConfigFile)
		if err != nil {
			log.Fatal(err)
		}
		c.DynamicConfig = *dc
	}

	if c.DSCP < 0 || c.DSCP > 63 {
		log.Fatalf("Unsupported DSCP value %v", c.DSCP)
	}

	if c.DomainNumber > 255 {
		log.Fatalf("Unsupported DomainNumber value %v", c.DomainNumber)
	}

	switch c.TimestampType {
	case timestamp.SW:
		log.Warning("Software timestamps greatly reduce the precision")
		fallthrough
	case timestamp.HW:
		log.Debugf("Using %s timestamps", c.TimestampType)
	default:
		log.Fatalf("Unrecognized timestamp type: %s", c.TimestampType)
	}

	c.IP = net.ParseIP(ipaddr)
	found, err := c.IfaceHasIP()
	if err != nil {
		log.Fatal(err)
	}
	if !found {
		log.Fatalf("IP '%s' is not found on interface '%s'", c.IP, c.Interface)
	}

	if c.DebugAddr != "" {
		log.Warningf("Staring profiler on %s", c.DebugAddr)
		go func() {
			log.Println(http.ListenAndServe(c.DebugAddr, nil))
		}()
	}

	log.Infof("UTC offset is: %v", c.UTCOffset)

	// Monitoring
	// Replace with your implementation of Stats
	st := stats.NewJSONStats(c.RecvWorkers)
	go st.Start(c.MonitoringPort)

	// drain check
	check := &drain.FileDrain{FileName: c.DrainFileName}
	checks := []drain.Drain{check}

	s := server.Server{
		Config: c,
		Stats:  st,
		Checks: checks,
	}

	if err := s.Start(); err != nil {
		log.Fatalf("Server run failed: %v", err)
	}
}
