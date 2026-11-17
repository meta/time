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
	"container/ring"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/facebook/time/fbclock"
	"github.com/facebook/time/ptp/linearizability"
)

// state of the daemon, guarded by mutex
type daemonState struct {
	sync.Mutex

	DataPoints                 *ring.Ring // DataPoints we collected from ptp4l
	mmms                       *ring.Ring // M values we calculated
	linearizabilityTestResults *ring.Ring // linearizability test results
	coefPPBs                   *ring.Ring // set of extrapolation coefficients for mean coefficient
	coefPPBsV3                 *ring.Ring // the same for v3, apart from v2's so v3 can't move v2's mean

	lastIngressTimeNS int64
	lastStoredData    atomic.Pointer[fbclock.Data]
}

// coefPPBRingSize is the number of extrapolation coefficients averaged into the
// mean coefficient: one per anchor over ~1s, the span validated to smooth
// extended holdover (up to 30s). It is sized against anchorInterval on purpose,
// decoupled from the ptp4l sampling interval that sizes the other rings, so an
// unrelated sampling knob can't silently retune the holdover extrapolation
// window.
const coefPPBRingSize = int(time.Second / anchorInterval)

func newDaemonState(ringSize int) *daemonState {
	s := &daemonState{
		DataPoints:                 ring.New(ringSize),
		mmms:                       ring.New(ringSize),
		linearizabilityTestResults: ring.New(ringSize),
		coefPPBs:                   ring.New(coefPPBRingSize),
		coefPPBsV3:                 ring.New(coefPPBRingSize),
	}
	// init ring buffers with nils
	for range ringSize {
		s.DataPoints.Value = nil
		s.DataPoints = s.DataPoints.Next()

		s.mmms.Value = nil
		s.mmms = s.mmms.Next()

		s.linearizabilityTestResults.Value = nil
		s.linearizabilityTestResults = s.linearizabilityTestResults.Next()
	}

	for range coefPPBRingSize {
		s.coefPPBs.Value = nil
		s.coefPPBs = s.coefPPBs.Next()
	}
	return s
}

func (s *daemonState) pushDataPoint(data *DataPoint) {
	s.Lock()
	defer s.Unlock()
	s.DataPoints.Value = data
	s.DataPoints = s.DataPoints.Next()
}

func (s *daemonState) takeDataPoint(n int) []*DataPoint {
	s.Lock()
	defer s.Unlock()
	result := []*DataPoint{}
	r := s.DataPoints.Prev()
	for range n {
		if r.Value == nil {
			continue
		}
		result = append(result, r.Value.(*DataPoint))
		r = r.Prev()
	}
	return result
}

func (s *daemonState) aggregateDataPointsMax(n int) *DataPoint {
	s.Lock()
	defer s.Unlock()
	d := &DataPoint{}
	r := s.DataPoints.Prev()
	for range n {
		if r.Value == nil {
			continue
		}
		dp := r.Value.(*DataPoint)
		if math.Abs(dp.MasterOffsetNS) > d.MasterOffsetNS {
			d.MasterOffsetNS = math.Abs(dp.MasterOffsetNS)
		}
		if math.Abs(dp.PathDelayNS) > d.PathDelayNS {
			d.PathDelayNS = math.Abs(dp.PathDelayNS)
		}
		if math.Abs(dp.FreqAdjustmentPPB) > d.FreqAdjustmentPPB {
			d.FreqAdjustmentPPB = math.Abs(dp.FreqAdjustmentPPB)
		}
		r = r.Prev()
	}
	return d
}

func (s *daemonState) pushM(data float64) {
	s.Lock()
	defer s.Unlock()
	s.mmms.Value = data
	s.mmms = s.mmms.Next()
}

func (s *daemonState) takeM(n int) []float64 {
	s.Lock()
	defer s.Unlock()
	result := []float64{}
	r := s.mmms.Prev()
	for range n {
		if r.Value == nil {
			continue
		}
		result = append(result, r.Value.(float64))
		r = r.Prev()
	}
	return result
}

func (s *daemonState) pushLinearizabilityTestResult(data linearizability.TestResult) {
	s.Lock()
	defer s.Unlock()
	s.linearizabilityTestResults.Value = data
	s.linearizabilityTestResults = s.linearizabilityTestResults.Next()
}

func (s *daemonState) takeLinearizabilityTestResult(n int) []linearizability.TestResult {
	s.Lock()
	defer s.Unlock()
	result := []linearizability.TestResult{}
	r := s.linearizabilityTestResults.Prev()
	for range n {
		if r.Value == nil {
			continue
		}
		result = append(result, r.Value.(linearizability.TestResult))
		r = r.Prev()
	}
	return result
}

func (s *daemonState) getMeanCoeffPPB(coef int64) int64 {
	return pushMeanCoeffPPB(&s.coefPPBs, coef)
}

// resetCoeffPPBsV3 empties v3's ring.
func (s *daemonState) resetCoeffPPBsV3() { s.coefPPBsV3 = ring.New(coefPPBRingSize) }

// getMeanCoeffPPBV3 is getMeanCoeffPPB on v3's ring.
func (s *daemonState) getMeanCoeffPPBV3(coef int64) int64 {
	return pushMeanCoeffPPB(&s.coefPPBsV3, coef)
}

// pushMeanCoeffPPB stores coef in the ring, over its oldest value once full, and returns
// the mean of what the ring holds: the one averaging v2 and v3 use, each on its own ring.
func pushMeanCoeffPPB(coefs **ring.Ring, coef int64) int64 {
	(*coefs).Value = coef
	*coefs = (*coefs).Next()

	var cnt, sumCoeff int64
	(*coefs).Do(func(val any) {
		if val == nil {
			return
		}

		v := val.(int64)
		cnt++
		sumCoeff += v
	})

	return sumCoeff / cnt
}

func (s *daemonState) getLastStoredData() *fbclock.Data {
	return s.lastStoredData.Load()
}

func (s *daemonState) setLastStoredData(d *fbclock.Data) {
	s.lastStoredData.Store(d)
}
