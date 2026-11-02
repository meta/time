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
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/facebook/time/fbclock/daemon"
	"github.com/facebook/time/fbclock/stats"
	ptp "github.com/facebook/time/ptp/protocol"

	log "github.com/sirupsen/logrus"
)

func main() {
	var (
		cfg            = &daemon.Config{}
		err            error
		cfgPath        string
		manageDevice   bool
		csvLog         bool
		csvPath        string
		logSampleRate  int
		verbose        bool
		monitoringPort int
	)

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "fbclock daemon\n")
		fmt.Fprintf(flag.CommandLine.Output(), "%s\n\nFlags:\n", daemon.MathHelp)
		flag.PrintDefaults()
	}

	flag.StringVar(&cfg.Iface, "iface", "eth0", "Network interface to use PHC device from. Used for linearizability tests as well. Must match what PTP client is configured to use")
	flag.StringVar(&cfg.PTPClientAddress, "ptpclientaddress", ptp.PTP4lSock, "Path to PTP client management address")
	flag.BoolVar(&cfg.SPTP, "sptp", false, "Connect to sptp instead ot ptp4l")
	flag.Float64Var(&cfg.MaxDriftRate, "max-drift-rate", daemon.DefaultMaxDriftRatePPM, "Maximum drift rate in PPM, the floor for chronyd's holdover multiplier")
	flag.IntVar(&monitoringPort, "monitoringport", 21041, "Port to run monitoring server on")
	flag.IntVar(&cfg.RingSize, "buffer", daemon.MathDefaultHistory, "Size of ring buffers, must be at least size of largest num of samples used in M and W formulas")
	flag.StringVar(&cfg.Math.M, "m", daemon.MathDefaultM, "Math expression for M")
	flag.StringVar(&cfg.Math.W, "w", daemon.MathDefaultW, "Math expression for W")
	flag.StringVar(&cfg.Math.Drift, "drift", daemon.MathDefaultDrift, "Math expression for Drift PPB")
	flag.DurationVar(&cfg.Interval, "i", time.Second, "Interval at which we talk to PTP client and update data in shm")
	flag.DurationVar(&cfg.LinearizabilityTestInterval, "I", time.Minute, "Interval at which we run linearizability tests. 0 means disabled")
	flag.DurationVar(&cfg.LinearizabilityTestMaxGMOffset, "o", 10*time.Microsecond, "Max offset between GMs before linearizability test considered failed")
	flag.DurationVar(&cfg.BootDelay, "b", 0, "Postpone startup by this time after boot")
	flag.BoolVar(&cfg.EnableDataV2, "v2", true, "Enable data v2 in shared memory region (default: true)")
	flag.BoolVar(&cfg.EnableDataV3, "v3", false, "Also write fbclock data v3 to /run/fbclock/data")
	flag.BoolVar(&cfg.GradualWindow, "gradualwindow", false, "Publish a widened window from the first sample after a restart (default off)")
	flag.StringVar(&cfgPath, "cfg", "", "Path to config")
	flag.BoolVar(&manageDevice, "manage", true, fmt.Sprintf("Manage device. This will setup %q as a copy of PHC device associated with given network interface", daemon.ManagedPTPDevicePath))
	flag.BoolVar(&csvLog, "csvlog", true, "Log all the metrics as CSV to log")
	flag.StringVar(&csvPath, "csvpath", "", "write CSV log into this file")
	flag.IntVar(&logSampleRate, "logsamplerate", 1, "Sample metrics logs at this rate. 0 means metrics logging is turned off. 1 means every sample is logged, 100 means roughly one in 100 samples will be logged")
	flag.BoolVar(&verbose, "verbose", false, "Verbose logging")

	flag.Parse()

	log.SetReportCaller(true)
	if verbose {
		log.SetLevel(log.DebugLevel)
	}
	if csvPath != "" && !csvLog {
		log.Fatal("'csvpath' flag requires 'csvlog' flag")
	}
	if cfgPath != "" {
		log.Warningf("using config from %s, flag values are ignored", cfgPath)
		cfg, err = daemon.ReadConfig(cfgPath)
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := cfg.EvalAndValidate(); err != nil {
		log.Fatal(err)
	}
	if manageDevice {
		if err := daemon.SetupDeviceDir(cfg.Iface); err != nil {
			log.Fatal(err)
		}
	}
	if err = cfg.PostponeStart(); err != nil {
		log.Errorf("failed to postpone startup: %v", err)
	}

	log.Debugf("Config: %+v", *cfg)

	// set up sample logging
	w := log.StandardLogger().Writer()
	defer w.Close()
	var l daemon.Logger = daemon.NewDummyLogger(w, logSampleRate)
	if csvLog {
		csvW := io.Writer(w)
		// set up logging of CSV samples to file
		if csvPath != "" {
			f, err := os.Create(csvPath)
			if err != nil {
				log.Fatal(err)
			}
			defer f.Close()
			// write both to stderr and file
			csvW = io.MultiWriter(w, f)
		}
		l = daemon.NewCSVLogger(csvW, logSampleRate)
	}
	s := stats.NewJSONStats()
	go s.Start(monitoringPort)
	d, err := daemon.New(cfg, s, l)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
