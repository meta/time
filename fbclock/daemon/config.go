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

package daemon

import (
	"fmt"
	"math"
	"os"
	"slices"
	"time"

	"github.com/facebook/time/fbclock"
	"github.com/facebook/time/phc/unix"
	log "github.com/sirupsen/logrus"
	yaml "gopkg.in/yaml.v2"
)

// DefaultMaxDriftRatePPM is -max-drift-rate's default, the floor for chronyd's
// holdover multiplier.
const DefaultMaxDriftRatePPM = 50

// Config represents configuration we expect to read from file
type Config struct {
	PTPClientAddress               string        // where should fbclock connect to
	RingSize                       int           // must be at least the size of N samples we use in expressions
	Math                           Math          // configuration for calculation we'll be doing
	Interval                       time.Duration // how often do we poll ptp4l and update data in shm
	Iface                          string        // network interface to use
	LinearizabilityTestInterval    time.Duration // perform the linearizability test every so often
	SPTP                           bool          // denotes whether we are running in sptp or ptp4l mode
	LinearizabilityTestMaxGMOffset time.Duration // max offset between GMs before linearizability test considered failed
	BootDelay                      time.Duration // postpone startup by this time after boot
	EnableDataV2                   bool          // enable fbclock data v2
	EnableDataV3                   bool          // also write fbclock data v3, /run/fbclock/data
	GradualWindow                  bool          // publish a widened window from the first sample after restart (default off)
	TimingStats                    bool          // report timing stats, for now how long data v2 writes keep the primary section mid-write (default off)
	MaxDriftRate                   float64       // floor for chronyd's holdover multiplier, in PPM
	// kFactors is the precomputed warm-up tolerance-factor table k[2..RingSize], looked up per tick.
	kFactors []float64
}

// EvalAndValidate makes sure config is valid and evaluates expressions for further use.
func (c *Config) EvalAndValidate() error {
	if c.PTPClientAddress == "" {
		return fmt.Errorf("bad config: 'ptpclientaddress'")
	}
	if c.RingSize <= 0 {
		return fmt.Errorf("bad config: 'ringsize' must be >0")
	}
	if c.Interval <= 0 || c.Interval > time.Minute {
		return fmt.Errorf("bad config: 'interval' must be between 0 and 1 minute")
	}

	if c.LinearizabilityTestInterval < 0 {
		return fmt.Errorf("bad config: 'test interval' must be positive")
	}

	if c.LinearizabilityTestMaxGMOffset < 0 {
		return fmt.Errorf("bad config: 'offset' must be positive")
	}
	if err := c.Math.Prepare(); err != nil {
		return err
	}
	// Fail closed: -gradualwindow relaxes the publish gate to n=2, so a custom -w without k would apply a
	// flat 4.0 to a 2-sample stddev. Require k in the W expression.
	if c.GradualWindow && !slices.Contains(c.Math.wExpr.Vars(), "k") {
		return fmt.Errorf("bad config: 'gradualwindow' requires the W expression to reference 'k' (got %q)", c.Math.W)
	}
	// with no floor a zero chronyd skew gives ntp a holdover rate of zero, and a bound that
	// never widens under-reports once chronyd stops answering
	if c.EnableDataV3 && (math.IsNaN(c.MaxDriftRate) || c.MaxDriftRate <= 0) {
		return fmt.Errorf("bad config: 'maxdriftrate' must be positive")
	}
	// the v3 file holds the holdover rate as Q16.16 ns/s, which saturates from 65.536 PPM up,
	// and readers refuse a saturated rate
	if c.EnableDataV3 && fbclock.FloatAsUint32(c.MaxDriftRate*nsPerSecondPerPPM) == math.MaxUint32 {
		return fmt.Errorf("bad config: 'maxdriftrate' must be below 65.536")
	}
	// Precompute the data-independent factor table once at parse; only used when GradualWindow is on.
	c.kFactors = precomputeGradualWindowFactors(c.RingSize, warmupConfidence)
	return nil
}

// gradualFactor returns the precomputed warm-up factor for n samples, falling back to coverageZP (4.0)
// when the table is absent (tests calling Math.Prepare directly) or n is outside [2, RingSize]. The
// flag-off path only ever hits n == RingSize, where the factor is 4.0, so it stays byte-identical.
func (c *Config) gradualFactor(n int) float64 {
	if n >= 2 && n < len(c.kFactors) {
		return c.kFactors[n]
	}
	return coverageZP
}

// PostponeStart postpones startup by BootDelay
func (c *Config) PostponeStart() error {
	uptime, err := uptime()
	if err != nil {
		return err
	}
	log.Debugf("system uptime: %s", uptime)

	if uptime < c.BootDelay {
		log.Infof("postponing startup by %s", c.BootDelay-uptime)
		time.Sleep(c.BootDelay - uptime)
	}

	return nil
}

// uptime returns system boot time
func uptime() (time.Duration, error) {
	var ts unix.Timespec

	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, err
	}

	return time.Duration(ts.Nano()), nil
}

// ReadConfig reads config and unmarshals it from yaml into Config
func ReadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Config{}
	err = yaml.UnmarshalStrict(data, &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}
