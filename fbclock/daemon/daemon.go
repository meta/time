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
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/facebook/time/fbclock"
	"github.com/facebook/time/fbclock/stats"

	"github.com/facebook/time/leapsectz"
	"github.com/facebook/time/ntp/chrony"
	"github.com/facebook/time/phc"
	"github.com/facebook/time/ptp/linearizability"
	ptp "github.com/facebook/time/ptp/protocol"
)

const (
	leapDurationS uint64 = 62500      // 17.36 hours https://chrony-project.org/doc/4.6/chrony.conf.html
	lastLeapS     uint64 = 1483228800 // 2017-01-01 00:00:00 UTC, when TAI-UTC went from 36s to 37s
	utcOffsetS           = int32(leapsectz.UTCOffset / time.Second)
	monPrefix     string = "linearizability."
	// nsPerSecondPerPPM converts a frequency error in PPM into ns of drift per
	// second: 1 PPM = 1e-6 s/s = 1000 ns/s.
	nsPerSecondPerPPM float64 = 1000.0
)

var errNotEnoughData = errors.New("not enough data points")
var errNoTestResults = errors.New("no test results")
var errNoPHC = errors.New("phc error")
var errCorrectness = errors.New("sanity checking data point error")

// defaultTargets is a list of targets if no available
var defaultTargets = []string{"::1", "::2", "::3"}

// lastLeapSmearing is the smearing window of the last leap second. Leap seconds
// are abolished, so clients are always past it.
var lastLeapSmearing = clockSmearing{
	// smearing starts at the leap second and runs leapDurationS, in TAI
	smearingStartS: lastLeapS + uint64(utcOffsetS-1),
	smearingEndS:   lastLeapS + leapDurationS + uint64(utcOffsetS-1),
	utcOffsetPreS:  utcOffsetS - 1,
	utcOffsetPostS: utcOffsetS,
}

var (
	// ptpDevicePath is where New and Run's re-check look for the PTP device; tests point it
	// elsewhere.
	ptpDevicePath = ManagedPTPDevicePath
	// ptpRecheckInterval is how often a host without PTP checks for it again: the pace at
	// which systemd restarts the sidecar after it exits. Tests shorten it.
	ptpRecheckInterval = 10 * time.Second
)

// DataPoint is what we store in DataPoint ring buffer
type DataPoint struct {
	// IngressTimeNS represents ingress time in NanoSeconds
	IngressTimeNS int64
	// MasterOffsetNS represents master offset in NanoSeconds
	MasterOffsetNS float64
	// PathDelayNS represents path delay in NanoSeconds
	PathDelayNS float64
	// FreqAdjustmentPPB represents freq adjustment in parts per billion
	FreqAdjustmentPPB float64
	// ClockAccuracyNS represents clock accurary in nanoseconds
	ClockAccuracyNS float64
	// ServoState represents the servo state from the PTP client
	ServoState int
	// ServoStateUnavailable marks a PTP client that reports no servo state (ptp4l).
	// Negative sense: an unset flag keeps the locked check on, so a fetcher that
	// forgets it fails closed.
	ServoStateUnavailable bool
}

// SanityCheck checks datapoint for correctness
func (d *DataPoint) SanityCheck() error {
	if d.IngressTimeNS == 0 {
		return fmt.Errorf("ingress time is 0")
	}
	if d.MasterOffsetNS == 0 {
		return fmt.Errorf("master offset is 0")
	}
	if d.PathDelayNS == 0 {
		return fmt.Errorf("path delay is 0")
	}
	if d.FreqAdjustmentPPB == 0 {
		return fmt.Errorf("frequency adjustment is 0")
	}
	if d.ClockAccuracyNS == 0 {
		return fmt.Errorf("clock accuracy is 0")
	}
	if time.Duration(d.ClockAccuracyNS) >= ptp.ClockAccuracyUnknown.Duration() {
		return fmt.Errorf("clock accuracy is unknown")
	}
	if !d.ServoStateUnavailable && d.ServoState != 2 { // servo.StateLocked
		return fmt.Errorf("servo state is %d, not locked", d.ServoState)
	}
	return nil
}

// DataFetcher is the data fetcher interface. Implementations embed it and
// implement only the methods their mode uses.
type DataFetcher interface {
	//function to gm data
	FetchGMs(cfg *Config) (targest []string, err error)
	//function to fetch stats
	FetchStats(cfg *Config) (*DataPoint, error)
	//function to fetch the time source's own error estimate, chronyd only
	FetchTracking(cfg *Config) (*chrony.Tracking, error)
}

// Daemon is a component of fbclock that
// runs continuously,
// talks to ptp4l,
// does the math
// and populates shared memory for client library to read from.
type Daemon struct {
	DataFetcher
	cfg   *Config
	state *daemonState
	stats stats.Server
	l     Logger

	// sources is what this host offers, decided once in New; v3's header lists exactly these
	sources fbclock.Sources

	// function to get PHC time from configured PHC device
	getPHCTime func() (time.Time, error)
	// getPHCAndSysTime returns PHC time, system time, system clock id, and the
	// PHC↔sys round-trip delay observed during the read (uncertainty bound).
	// Uses the MONOTONIC_RAW preferred path (existing behavior) for the primary
	// snapshot that fbclock_gettime consumes.
	getPHCAndSysTime func() (time.Time, time.Time, uint32, time.Duration, error)
	// getPHCAndSysTimeRealtime is a parallel snapshot taken with CLOCK_REALTIME.
	// Populates the realtime anchor used by fbclock_gettime_past so callers can
	// translate kernel SO_TIMESTAMPING software timestamps (always REALTIME)
	// without affecting the precision of the primary anchor.
	getPHCAndSysTimeRealtime func() (time.Time, time.Time, time.Duration, error)
	// function to get PHC freq from configured PHC device
	getPHCFreqPPB func() (float64, error)
}

// minRingSize calculate how many DataPoint we need to have in a ring buffer
// in order to provide aggregate values over 1 minute
func minRingSize(configuredRingSize int, interval time.Duration) int {
	size := configuredRingSize
	if time.Duration(size)*interval < time.Minute {
		size = int(math.Ceil(float64(time.Minute) / float64(interval)))
	}
	return size
}

type clockSmearing struct {
	smearingStartS uint64 // time (TAI) when smearing starts
	smearingEndS   uint64 // time (TAI) when smearing ends
	utcOffsetPreS  int32  // DTAI offset prior to Leap Second Event Time
	utcOffsetPostS int32  // DTAI offset post Leap Second Event Time
}

// noTestResults generates a map of error test results
func noTestResults(targets []string) map[string]linearizability.TestResult {
	r := map[string]linearizability.TestResult{}
	for _, target := range targets {
		r[target] = linearizability.SPTPHTTPTestResult{Error: errNoTestResults}
	}
	return r
}

// setupPHC opens the PHC device backing iface and installs the clock accessors
// that read it. The device file stays open for the lifetime of the Daemon.
func (s *Daemon) setupPHC(iface string) error {
	phcDevice, err := phc.IfaceToPHCDevice(iface)
	if err != nil {
		return fmt.Errorf("finding PHC device for %q: %w", iface, err)
	}

	// Keep file open for the lifetime of the fbclock
	f, err := os.OpenFile(phcDevice, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	dev := phc.FromFile(f)

	// function to get time from phc
	s.getPHCTime = func() (time.Time, error) { return dev.Time() }
	s.getPHCAndSysTime = func() (time.Time, time.Time, uint32, time.Duration, error) {
		// Primary anchor: uses the existing MONOTONIC_RAW-preferred path so that
		// fbclock_gettime behavior is unchanged. Falls back to REALTIME if the
		// kernel doesn't accept MONO_RAW in the ioctl.
		data, err := dev.ReadSysoffExtended()
		if err != nil {
			return time.Time{}, time.Time{}, 0, 0, err
		}
		best := data.BestSample()
		return best.PHCTime, best.SysTime, uint32(best.SysClockID), best.Delay, nil //nolint:gosec
	}
	s.getPHCAndSysTimeRealtime = func() (time.Time, time.Time, time.Duration, error) {
		// Realtime anchor in CLOCK_REALTIME, only for fbclock_gettime_past.
		data, err := dev.ReadSysoffExtendedRealTimeClock()
		if err != nil {
			return time.Time{}, time.Time{}, 0, err
		}
		best := data.BestSample()
		return best.PHCTime, best.SysTime, best.Delay, nil
	}
	s.getPHCFreqPPB = func() (float64, error) { return dev.FreqPPB() }
	return nil
}

// New creates new fbclock-daemon. It runs PTP only where ManagedPTPDevicePath exists and
// cfg.Iface has a PHC, and offers ntp everywhere.
func New(cfg *Config, stats stats.Server, l Logger) (*Daemon, error) {
	// we need at least 1m of samples for aggregate values
	effectiveRingSize := minRingSize(cfg.RingSize, cfg.Interval)
	s := &Daemon{
		stats: stats,
		state: newDaemonState(effectiveRingSize),
		cfg:   cfg,
		l:     l,
	}
	if cfg.SPTP {
		s.DataFetcher = &HTTPFetcher{}
	} else {
		s.DataFetcher = &SockFetcher{}
	}

	// chrony runs on every host the sidecar runs on
	s.sources = fbclock.SourceNTP
	// the device marks a PTP host, which a PHC alone doesn't: non-PTP NICs have one too
	_, err := os.Stat(ptpDevicePath)
	if err == nil {
		err = s.setupPHC(cfg.Iface)
	}
	if err != nil {
		if !cfg.EnableDataV3 {
			return nil, fmt.Errorf("nothing to write without PTP and -v3: %w", err)
		}
		log.Infof("running without PTP, writing only ntp to %s: %v", fbclock.DataPath, err)
		return s, nil
	}
	s.sources |= fbclock.SourcePTP | fbclock.SourcePRTC
	log.Infof("running with PTP: %s exists and %s has a PHC", ptpDevicePath, cfg.Iface)
	// calculated values
	s.stats.SetCounter("m_ns", 0)
	s.stats.SetCounter("w_ns", 0)
	s.stats.SetCounter("drift_ppb", 0)
	// error counters
	s.stats.SetCounter("data_error", 0)
	s.stats.SetCounter("processing_error", 0)
	s.stats.SetCounter("data_sanity_check_error", 0)
	s.stats.SetCounter("monotonictime_error", 0)
	s.stats.SetCounter("phc_read_error", 0)
	// values collected from ptp4l
	s.stats.SetCounter("ingress_time_ns", 0)
	s.stats.SetCounter("master_offset_ns", 0)
	s.stats.SetCounter("path_delay_ns", 0)
	s.stats.SetCounter("freq_adj_ppb", 0)
	s.stats.SetCounter("clock_accuracy_ns", 0)
	// aggregated values
	s.stats.SetCounter("master_offset_ns.60.abs_max", 0)
	s.stats.SetCounter("path_delay_ns.60.abs_max", 0)
	s.stats.SetCounter("freq_adj_ppb.60.abs_max", 0)
	return s, nil
}

func (s *Daemon) calcW() (float64, error) {
	lastN := s.state.takeDataPoint(s.cfg.RingSize)
	params := prepareMathParameters(lastN)
	logSample := &LogSample{
		MasterOffsetNS:          params["offset"][0],
		MasterOffsetMeanNS:      mean(params["offset"]),
		MasterOffsetStddevNS:    stddev(params["offset"]),
		PathDelayNS:             params["delay"][0],
		PathDelayMeanNS:         mean(params["delay"]),
		PathDelayStddevNS:       stddev(params["delay"]),
		FreqAdjustmentPPB:       params["freq"][0],
		FreqAdjustmentMeanPPB:   mean(params["freq"]),
		FreqAdjustmentStddevPPB: stddev(params["freq"]),
		ClockAccuracyMean:       mean(params["clockaccuracy"]),
	}
	mRaw, err := s.cfg.Math.mExpr.Evaluate(mapOfInterface(params))
	if err != nil {
		return 0, err
	}
	m := mRaw.(float64)
	logSample.MeasurementNS = m
	s.stats.SetCounter("m_ns", int64(m))

	// push m to ring buffer
	s.state.pushM(m)

	ms := s.state.takeM(s.cfg.RingSize)
	minN := s.cfg.RingSize
	if s.cfg.GradualWindow {
		// Publish from n=2 (skip n=1: stddev is 0). len(ms) <= RingSize, so flag-off keeps today's gate.
		minN = 2
	}
	if len(ms) < minN {
		return 0, fmt.Errorf("%w getting W: want >= %d, got %d", errNotEnoughData, minN, len(ms))
	}

	n := len(ms)
	// k is the precomputed factor for this sample count: coverageZP (4.0) at the full ring, so steady
	// state and the flag-off path stay byte-identical. n and k are always injected; govaluate ignores
	// vars a formula does not reference.
	parameters := map[string]any{
		"m": ms,
		"n": float64(n),
		"k": s.cfg.gradualFactor(n),
	}
	logSample.MeasurementMeanNS = mean(ms)
	logSample.MeasurementStddevNS = stddev(ms)

	wRaw, err := s.cfg.Math.wExpr.Evaluate(parameters)
	if err != nil {
		return 0, err
	}
	w := wRaw.(float64)
	logSample.WindowNS = w
	if err := s.l.Log(logSample); err != nil {
		log.Errorf("failed to log sample: %v", err)
	}
	s.stats.SetCounter("w_ns", int64(w))
	return w, nil
}

func (s *Daemon) calcDriftPPB() (float64, error) {
	lastN := s.state.takeDataPoint(s.cfg.RingSize)
	minN := s.cfg.RingSize
	if s.cfg.GradualWindow {
		// Relax with the W gate: drift has no factor, but if it still required a full ring doWork would
		// swallow the error and publish nothing. At n=2 freqchangeabs has one value.
		minN = 2
	}
	if len(lastN) < minN {
		return 0, fmt.Errorf("%w calculating drift: want >= %d, got %d", errNotEnoughData, minN, len(lastN))
	}
	params := prepareMathParameters(lastN)
	driftRaw, err := s.cfg.Math.driftExpr.Evaluate(mapOfInterface(params))
	if err != nil {
		return 0, err
	}
	drift := driftRaw.(float64)
	return drift, nil
}

// These are the raw gauges that predate per-field validation. Derived gauges
// below update only after calculateSHMData accepts the complete sample.
func (s *Daemon) publishRawDataPointGauges(data *DataPoint) {
	if data.IngressTimeNS != 0 {
		s.stats.SetCounter("ingress_time_ns", data.IngressTimeNS)
	}
	if data.MasterOffsetNS != 0 {
		s.stats.SetCounter("master_offset_ns", int64(data.MasterOffsetNS))
	}
	if data.PathDelayNS != 0 {
		s.stats.SetCounter("path_delay_ns", int64(data.PathDelayNS))
	}
	if data.FreqAdjustmentPPB != 0 {
		s.stats.SetCounter("freq_adj_ppb", int64(data.FreqAdjustmentPPB))
	}
	// Zero means the client has no usable accuracy bound for this tick.
	s.stats.SetCounter("clock_accuracy_ns", int64(data.ClockAccuracyNS))
}

func (s *Daemon) calculateSHMData(data *DataPoint) (*fbclock.Data, error) {
	if err := data.SanityCheck(); err != nil {
		s.stats.UpdateCounterBy("data_sanity_check_error", 1)
		return nil, fmt.Errorf("%w: %w", errCorrectness, err)
	}
	s.stats.SetCounter("data_sanity_check_error", 0)

	// store DataPoint in ring buffer
	s.state.pushDataPoint(data)

	// calculate W
	w, err := s.calcW()
	if err != nil {
		return nil, fmt.Errorf("calculating W: %w", err)
	}

	wUint := uint64(w)
	if wUint == 0 {
		return nil, fmt.Errorf("value of W is 0")
	}
	// drift is in PPB, parts per billion.
	// 1ns = 1/billions of second, so we can just say that
	// hValue is measured in ns per second
	hValue, err := s.calcDriftPPB()
	if err != nil {
		return nil, fmt.Errorf("calculating drift: %w", err)
	}
	s.stats.SetCounter("drift_ppb", int64(hValue))

	clockSmearing := lastLeapSmearing
	return &fbclock.Data{
		IngressTimeNS:        data.IngressTimeNS,
		ErrorBoundNS:         wUint,
		HoldoverMultiplierNS: hValue,
		SmearingStartS:       clockSmearing.smearingStartS,
		SmearingEndS:         clockSmearing.smearingEndS,
		UTCOffsetPreS:        clockSmearing.utcOffsetPreS,
		UTCOffsetPostS:       clockSmearing.utcOffsetPostS,
	}, nil
}

func (s *Daemon) doWork(shm *fbclock.Shm, data *DataPoint) error {
	// push stats
	s.publishRawDataPointGauges(data)
	// try and calculate how long ago was the ingress time
	// use clock_gettime as the fastest and widely available method
	phcTime, err := s.getPHCTime()
	if err != nil {
		return fmt.Errorf("failed to get PHC time from %s: %w", s.cfg.Iface, errors.Join(errNoPHC, err))
	}

	if data.IngressTimeNS > 0 {
		s.state.lastIngressTimeNS = data.IngressTimeNS
	}
	it := s.state.lastIngressTimeNS
	if it > 0 {
		timeSinceIngress := phcTime.UnixNano() - it
		log.Debugf("Time since ingress: %dns", timeSinceIngress)
	} else {
		log.Warning("No data for time since ingress")
	}
	// store everything in shared memory
	d, err := s.calculateSHMData(data)
	if err != nil {
		if errors.Is(err, errNotEnoughData) {
			log.Warning(err)
			return nil
		}
		return err
	}
	s.state.setLastStoredData(d)
	if err := fbclock.StoreFBClockDataV1(shm.File.Fd(), *d); err != nil {
		return err
	}
	// aggregated stats over 1 minute
	maxDp := s.state.aggregateDataPointsMax(minRingSize(s.cfg.RingSize, s.cfg.Interval))
	s.stats.SetCounter("master_offset_ns.60.abs_max", int64(maxDp.MasterOffsetNS))
	s.stats.SetCounter("path_delay_ns.60.abs_max", int64(maxDp.PathDelayNS))
	s.stats.SetCounter("freq_adj_ppb.60.abs_max", int64(maxDp.FreqAdjustmentPPB))
	return nil
}

func targetsDiff(oldTargets []string, targets []string) (added []string, removed []string) {
	m := map[string]bool{}
	for _, k := range oldTargets {
		m[k] = true
	}
	for _, k := range targets {
		if m[k] {
			delete(m, k)
		} else {
			added = append(added, k)
		}
	}
	for k := range m {
		removed = append(removed, k)
	}
	return
}

func (s *Daemon) runLinearizabilityTests(ctx context.Context) {
	testers := map[string]linearizability.Tester{}
	oldTargets := []string{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := new(sync.Mutex)

	ticker := time.NewTicker(s.cfg.LinearizabilityTestInterval)
	defer ticker.Stop()
	for ; true; <-ticker.C { // first run without delay, then at interval
		eg := new(errgroup.Group)
		currentResults := map[string]linearizability.TestResult{}
		targets, err := s.FetchGMs(s.cfg)
		if err != nil {
			log.Errorf("getting linearizability test targets: %v", err)
			if len(oldTargets) > 0 {
				linearizability.ProcessMonitoringResults(monPrefix, noTestResults(oldTargets), s.stats)
			} else {
				linearizability.ProcessMonitoringResults(monPrefix, noTestResults(defaultTargets), s.stats)
			}
			continue
		}
		log.Debugf("targets: %v, err: %v", targets, err)
		// log when set of targets changes
		added, removed := targetsDiff(oldTargets, targets)
		if len(added) > 0 || len(removed) > 0 {
			log.Infof("new set of linearizability test targets. Added: %v, Removed: %v. Resulting set: %v", added, removed, targets)
			// we never remove testers, as stopping background listener goroutine is not easy
			oldTargets = targets
		}

		for _, server := range targets {
			log.Debugf("talking to %s", server)
			lt, found := testers[server]
			if !found {
				if s.cfg.SPTP {
					lt, err = linearizability.NewSPTPHTTPTester(server, fmt.Sprintf("http://%s/", s.cfg.PTPClientAddress), s.cfg.LinearizabilityTestMaxGMOffset)
				} else {
					lt, err = linearizability.NewPTPTester(server, s.cfg.Iface, linearizability.IEEE1588)
				}
				if err != nil {
					log.Errorf("creating tester: %v", err)
					continue
				}
				testers[server] = lt
			}
			eg.Go(func() error {
				res := lt.RunTest(ctx)
				m.Lock()
				currentResults[server] = res
				m.Unlock()
				return nil
			})
		}
		err = eg.Wait()
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error(err)
		}

		// get result, log it and push it into ring buffer
		for _, res := range currentResults {
			good, err := res.Good()
			if err != nil {
				log.Error(res.Explain())
				continue
			}
			if !good {
				log.Warning(res.Explain())
			}
			log.Debugf("got linearizability result: %s", res.Explain())
			s.state.pushLinearizabilityTestResult(res)
		}
		// add stats from linearizability checks
		linearizability.ProcessMonitoringResults(monPrefix, currentResults, s.stats)
	}
}

// calcCoeffPPB computes the PHC-vs-sysclock rate ratio in PPB between the prev
// and cur snapshots of a single anchor section.
func calcCoeffPPB(prev, cur *fbclock.DataV2) (int64, error) {
	return coeffPPB(prev.PHCTimeNS, prev.SysclockTimeNS, cur.PHCTimeNS, cur.SysclockTimeNS)
}

// calcCoeffPPBV3 is calcCoeffPPB for v3's records, where the PHC time is RefTimeNS.
func calcCoeffPPBV3(prev, cur *fbclock.DataClock) (int64, error) {
	return coeffPPB(prev.RefTimeNS, prev.SysclockTimeNS, cur.RefTimeNS, cur.SysclockTimeNS)
}

// coeffPPB computes the ratio for calcCoeffPPB and calcCoeffPPBV3 from two anchors' PHC and
// system clock times, so v2 and v3 can't compute it differently.
func coeffPPB(prevPHCNS, prevSysNS, curPHCNS, curSysNS int64) (int64, error) {
	if prevSysNS == 0 {
		// first run, no previous data
		return 0, nil
	}
	// calculate the ratio of PHC time to system time (how faster (or slower) PHC ticks compared to system time)
	mCoeff := float64(curPHCNS-prevPHCNS) / float64(curSysNS-prevSysNS)
	// calculate the ratio in parts per billion (PPB)
	coefPPB := int64(mCoeff*float64(time.Second) - float64(time.Second))
	// check continuity of extrapolated PHC time
	sysTimeDiff := curSysNS - prevSysNS - 1
	phcTimeFixed := prevPHCNS + sysTimeDiff + coefPPB*sysTimeDiff/time.Second.Nanoseconds()
	var err error
	if phcTimeFixed > curPHCNS {
		err = fmt.Errorf("PHC time is not monotonic: %d > %d", phcTimeFixed, curPHCNS)
	}
	return coefPPB, err
}

// meanCoeffPPB returns 0 on first sample and mean coefficient in other cases
func (s *Daemon) meanCoeffPPB(prev, cur *fbclock.DataV2) int64 {
	if prev.SysclockTimeNS == 0 {
		return 0
	}
	coefPPB, err := calcCoeffPPB(prev, cur)
	if err != nil {
		log.Warning(err)
		s.stats.UpdateCounterBy("monotonictime_error", 1)
	}
	return s.state.getMeanCoeffPPB(coefPPB)
}

// meanCoeffPPBV3 is meanCoeffPPB for v3's records, on v3's own ring and without v2's counter.
func (s *Daemon) meanCoeffPPBV3(prev, cur *fbclock.DataClock) int64 {
	if prev.SysclockTimeNS == 0 {
		return 0
	}
	coefPPB, err := calcCoeffPPBV3(prev, cur)
	if err != nil {
		log.Warning(err)
	}
	return s.state.getMeanCoeffPPBV3(coefPPB)
}

// openDataV3 opens the v3 file at path for -v3, with a section for each of the host's sources.
// It returns nil when -v3 is off or the open fails, which stops only a host without PTP.
func (s *Daemon) openDataV3(path string) *fbclock.DataFile {
	if !s.cfg.EnableDataV3 {
		return nil
	}
	sections := map[fbclock.Sources]fbclock.DataSectionConfig{}
	for _, source := range []fbclock.Sources{fbclock.SourcePTP, fbclock.SourcePRTC, fbclock.SourceNTP} {
		if s.sources&source != 0 {
			sections[source] = fbclock.DataSectionConfig{}
		}
	}
	v3, err := fbclock.OpenFBClockDataCustom(path, sections)
	if err != nil {
		log.Errorf("not writing fbclock data v3: %v", err)
		return nil
	}
	log.Infof("writing fbclock data v3 to %s", path)
	return v3
}

// readDelayNS is a PHC read's delay, which the bound grows by. It's negative only when the
// system clock steps back mid-read, and then the reading can't anchor a record.
func readDelayNS(delay time.Duration) (uint64, error) {
	if delay < 0 {
		return 0, fmt.Errorf("negative PHC read delay %v", delay)
	}
	return uint64(delay), nil
}

// storePTPDataV3 stores a PTP record, the PTP loop's last sample anchored on a PHC reading,
// in v3's ptp or prtc section. Errors are only logged.
func storePTPDataV3(v3 *fbclock.DataFile, source fbclock.Sources, clock fbclock.DataClock) {
	if err := v3.Store(source, clock); err != nil {
		log.Errorf("writing %s to %s: %v", fbclock.DataSectionName(source), v3.Path, err)
	}
}

// populatePTPDataV3 writes v3's ptp and prtc sections every 10 ms: the PTP loop's last
// sample anchored on this loop's own PHC readings, as populateDataV2 does for v2.
func (s *Daemon) populatePTPDataV3(v3 *fbclock.DataFile) {
	prevPTP := fbclock.DataClock{}
	prevPRTC := fbclock.DataClock{}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for ; true; <-ticker.C { // first run without delay, then at interval
		d := s.state.getLastStoredData()
		if d == nil {
			continue
		}
		snapshot := *d
		phcTime, sysTime, clockID, phcReadDelay, err := s.getPHCAndSysTime()
		var delayNS uint64
		if err == nil {
			delayNS, err = readDelayNS(phcReadDelay)
		}
		if err != nil {
			log.Errorf("reading PHC time from %s for fbclock data v3: %v", s.cfg.Iface, err)
			continue
		}
		ptp := fbclock.DataClock{
			ClockID:              clockID,
			RefTimeNS:            phcTime.UnixNano(),
			SysclockTimeNS:       sysTime.UnixNano(),
			IngressTimeNS:        snapshot.IngressTimeNS,
			ErrorBoundNS:         snapshot.ErrorBoundNS + delayNS,
			HoldoverMultiplierNS: snapshot.HoldoverMultiplierNS,
		}
		if prevPTP.SysclockTimeNS != 0 && ptp.ClockID != prevPTP.ClockID {
			// phc fell back to another base clock, so the old anchor and coefficients don't apply
			prevPTP = fbclock.DataClock{}
			s.state.resetCoeffPPBsV3()
		}
		ptp.CoefPPB = s.meanCoeffPPBV3(&prevPTP, &ptp)
		prevPTP = ptp
		storePTPDataV3(v3, fbclock.SourcePTP, ptp)

		// a CLOCK_REALTIME primary already is the realtime anchor
		if clockID == unix.CLOCK_REALTIME {
			storePTPDataV3(v3, fbclock.SourcePRTC, ptp)
			continue
		}
		phcTimeRT, sysTimeRT, phcReadDelayRT, err := s.getPHCAndSysTimeRealtime()
		var delayNSRT uint64
		if err == nil {
			delayNSRT, err = readDelayNS(phcReadDelayRT)
		}
		if err != nil {
			log.Warningf("reading PHC time (REALTIME anchor) from %s for fbclock data v3: %v", s.cfg.Iface, err)
			continue
		}
		prtc := ptp
		prtc.ClockID = unix.CLOCK_REALTIME
		prtc.RefTimeNS = phcTimeRT.UnixNano()
		prtc.SysclockTimeNS = sysTimeRT.UnixNano()
		prtc.ErrorBoundNS = snapshot.ErrorBoundNS + delayNSRT
		if prtc.CoefPPB, err = calcCoeffPPBV3(&prevPRTC, &prtc); err != nil {
			log.Warning(err)
		}
		prevPRTC = prtc
		storePTPDataV3(v3, fbclock.SourcePRTC, prtc)
	}
}

// ntpSample is the last good chronyd poll, which the ntp section is rewritten from.
type ntpSample struct {
	ingressTimeNS        int64
	errorBoundNS         uint64
	holdoverMultiplierNS float64
}

// pollChrony fetches chronyd's tracking and turns it into the ntp section's bound, with
// sysNow, the system clock read just before, as the ingress.
func (s *Daemon) pollChrony(fetcher *ChronyFetcher, sysNow time.Time) (*ntpSample, error) {
	tracking, err := fetcher.FetchTracking(s.cfg)
	if err != nil {
		return nil, err
	}
	errorBoundNS, holdoverNS, err := ChronyBound(tracking, s.cfg.MaxDriftRate)
	if err != nil {
		return nil, err
	}
	return &ntpSample{
		ingressTimeNS:        sysNow.Add(leapsectz.UTCOffset).UnixNano(),
		errorBoundNS:         errorBoundNS,
		holdoverMultiplierNS: holdoverNS,
	}, nil
}

// storeNTPDataV3 writes sample to v3's ntp section, anchored on the system clock reading now.
func storeNTPDataV3(v3 *fbclock.DataFile, sample *ntpSample, now time.Time) error {
	// the system clock is both anchor (in TAI) and base, so the coefficient is 0
	return v3.Store(fbclock.SourceNTP, fbclock.DataClock{
		ClockID:              unix.CLOCK_REALTIME,
		RefTimeNS:            now.Add(leapsectz.UTCOffset).UnixNano(),
		SysclockTimeNS:       now.UnixNano(),
		IngressTimeNS:        sample.ingressTimeNS,
		ErrorBoundNS:         sample.errorBoundNS,
		HoldoverMultiplierNS: sample.holdoverMultiplierNS,
	})
}

// populateNTPDataV3 writes v3's ntp section until ctx is done: it polls chronyd every -i and
// rewrites the section every 10 ms from the last good poll, or refused under a local reference.
func (s *Daemon) populateNTPDataV3(ctx context.Context, fetcher *ChronyFetcher, v3 *fbclock.DataFile) {
	var last atomic.Pointer[ntpSample]
	var wg sync.WaitGroup
	defer wg.Wait()
	// its own goroutine, so a slow or dead chronyd never delays a write
	wg.Go(func() {
		// a host whose chronyd follows the PHC stays that way, so that's logged once
		var followedLocal bool
		ticker := time.NewTicker(s.cfg.Interval)
		defer ticker.Stop()
		for {
			// read before the fetch, so the bound ages from no later than the instant chronyd
			// describes; not RefTime, whose age RootDispersion already covers
			sample, err := s.pollChrony(fetcher, time.Now())
			local := errors.Is(err, errLocalReference)
			if err != nil && (!local || !followedLocal) {
				log.Errorf("polling chronyd for fbclock data v3: %v", err)
			}
			followedLocal = local
			switch {
			case err == nil:
				last.Store(sample)
			case local:
				// chronyd steers the clock to a reference nothing checks, so no NTP sample bounds it,
				// this run's or one a previous run left in the file: write a bound readers refuse
				last.Store(&ntpSample{errorBoundNS: math.MaxUint32})
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// nothing before the first good poll; after it, holdover from its ingress
		if sample := last.Load(); sample != nil {
			if err := storeNTPDataV3(v3, sample, time.Now()); err != nil {
				log.Errorf("writing ntp to %s: %v", v3.Path, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// populateDataV2 populates fbclock.DataV2 with data from fbclock.Data plus calculated values
func (s *Daemon) populateDataV2(shmv2 *fbclock.Shm) {
	prevPrimary := fbclock.DataV2{}
	prevRealtime := fbclock.DataV2{}

	fastTicker := time.NewTicker(10 * time.Millisecond)
	defer fastTicker.Stop()
	for ; true; <-fastTicker.C { // first run without delay, then at interval
		d := s.state.getLastStoredData()
		if d == nil {
			// we need a copy of the latest v1 data to fill in parts of v2 data
			// the pointer dereference is needed to avoid partial reads of the data
			continue
		}
		curData := *d
		phcTime, sysTime, clockID, phcReadDelay, err := s.getPHCAndSysTime()
		if err != nil {
			log.Errorf("reading PHC time from %s: %v", s.cfg.Iface, err)
			s.stats.UpdateCounterBy("phc_read_error", 1)
			continue
		}
		primary := fbclock.DataV2{
			IngressTimeNS:        curData.IngressTimeNS,
			ErrorBoundNS:         curData.ErrorBoundNS + uint64(phcReadDelay.Nanoseconds()), //nolint:gosec
			HoldoverMultiplierNS: curData.HoldoverMultiplierNS,
			SmearingStartS:       curData.SmearingStartS,
			UTCOffsetPreS:        int16(curData.UTCOffsetPreS),  //nolint:gosec
			UTCOffsetPostS:       int16(curData.UTCOffsetPostS), //nolint:gosec
			PHCTimeNS:            phcTime.UnixNano(),
			SysclockTimeNS:       sysTime.UnixNano(),
			ClockID:              clockID,
		}
		primary.CoefPPB = s.meanCoeffPPB(&prevPrimary, &primary)
		prevPrimary = primary
		// Publish the primary section right away, before the slower REALTIME
		// read below, so its anchor isn't aged by that read.
		if err := fbclock.StoreFBClockDataV2(shmv2.File.Fd(), primary); err != nil {
			log.Errorf("writing dataV2 to shm: %v", err)
		}

		// The REALTIME anchor section is only needed on hosts whose primary is
		// MONOTONIC_RAW. When the primary is already REALTIME, gettime_past uses
		// it directly, so we skip the second ioctl and don't write the section.
		if clockID != unix.CLOCK_REALTIME {
			phcTimeRT, sysTimeRT, phcReadDelayRT, errRT := s.getPHCAndSysTimeRealtime()
			if errRT != nil {
				// Tolerate failure: the primary section still publishes.
				log.Warningf("reading PHC time (REALTIME anchor) from %s: %v", s.cfg.Iface, errRT)
				s.stats.UpdateCounterBy("phc_read_error", 1)
			} else {
				// Start from the primary so the host-level fields (ingress,
				// holdover, utc, smearing) are shared, then override with the
				// REALTIME anchor and its own independent error bound.
				rt := primary
				rt.ClockID = unix.CLOCK_REALTIME
				rt.PHCTimeNS = phcTimeRT.UnixNano()
				rt.SysclockTimeNS = sysTimeRT.UnixNano()
				rt.ErrorBoundNS = curData.ErrorBoundNS + uint64(phcReadDelayRT.Nanoseconds()) //nolint:gosec
				if rt.CoefPPB, errRT = calcCoeffPPB(&prevRealtime, &rt); errRT != nil {
					log.Warning(errRT)
					s.stats.UpdateCounterBy("monotonictime_error", 1)
				}
				prevRealtime = rt
				if err := fbclock.StoreFBClockDataRealtime(shmv2.File.Fd(), rt); err != nil {
					log.Errorf("writing realtime dataV2 to shm: %v", err)
				}
			}
		}
	}
}

func sdNotify() error {
	s, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if !s && err != nil {
		return err
	} else if !s {
		log.Warn("sd_notify is not supported")
	} else {
		log.Info("sd_notify event sent")
	}
	return nil
}

// ptpAvailable reports whether New would now run PTP: the device exists and -iface's PHC opens.
func (s *Daemon) ptpAvailable() bool {
	if _, err := os.Stat(ptpDevicePath); err != nil {
		return false
	}
	return s.setupPHC(s.cfg.Iface) == nil
}

// waitForPTP checks every ptpRecheckInterval whether New would now run PTP, and returns an
// error once it would, so the process exits and systemd's restart runs PTP. It returns nil if
// ctx is done first.
func (s *Daemon) waitForPTP(ctx context.Context) error {
	ticker := time.NewTicker(ptpRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if s.ptpAvailable() {
			return errors.New("PTP is available now: exiting so the restart runs it")
		}
	}
}

// Run writes v3's ntp on every host and, where New found PTP, everything PTP writes.
func (s *Daemon) Run(ctx context.Context) error {
	// v3 isn't closed: the loops write to its mapping until the process exits
	v3 := s.openDataV3(fbclock.DataPath)
	if v3 != nil {
		go s.populateNTPDataV3(ctx, &ChronyFetcher{}, v3)
	}
	if s.sources&fbclock.SourcePTP == 0 {
		if v3 == nil {
			return errors.New("nothing to write without PTP: fbclock data v3 didn't open")
		}
		if err := sdNotify(); err != nil {
			log.Error(err)
		}
		// PTP can come later: the link once udev makes it, the PHC once it opens
		return s.waitForPTP(ctx)
	}

	shm, err := fbclock.OpenFBClockSHMv1()
	if err != nil {
		return fmt.Errorf("opening fbclock shm: %w", err)
	}
	defer shm.Close()

	if s.cfg.LinearizabilityTestInterval != 0 {
		go s.runLinearizabilityTests(ctx)
	}

	if v3 != nil {
		go s.populatePTPDataV3(v3)
	}

	if s.cfg.EnableDataV2 {
		shmv2, err := fbclock.OpenFBClockSHMv2()
		if err != nil {
			return fmt.Errorf("opening fbclock shm v2: %w", err)
		}
		defer shmv2.Close()
		go s.populateDataV2(shmv2)
	}

	if err := sdNotify(); err != nil {
		log.Error(err)
	}

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for ; true; <-ticker.C { // first run without delay, then at interval
		data, err := s.FetchStats(s.cfg)
		if err != nil {
			log.Error(err)
			s.stats.UpdateCounterBy("data_error", 1)
			continue
		}
		s.stats.SetCounter("data_error", 0)
		// get PHC freq adjustment
		freqPPB, err := s.getPHCFreqPPB()
		if err != nil {
			return err
		}
		data.FreqAdjustmentPPB = freqPPB
		if err := s.doWork(shm, data); err != nil {
			if errors.Is(err, errNoPHC) {
				return err
			} else if errors.Is(err, errCorrectness) {
				log.Warning(err)
			} else {
				log.Error(err)
			}
			s.stats.UpdateCounterBy("processing_error", 1)
			continue
		}
		s.stats.SetCounter("processing_error", 0)
	}
	return nil
}
