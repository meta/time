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

package fbclock

import (
	"time"
)

// Update processes result of GetTime call and updates Stats
func (s *StatsCollector) Update(tt *TrueTime, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Requests++
	if err != nil {
		s.stats.Errors++
		if s.errCauses == nil {
			s.errCauses = map[string]int64{}
		}
		cause := err.Error()
		if _, seen := s.errCauses[cause]; !seen && len(s.errCauses) >= maxErrorCauses-1 {
			cause = otherErrorCauses
		}
		s.errCauses[cause]++
		return
	}
	wou := tt.Latest.Sub(tt.Earliest)
	s.wouSum += int64(wou)
	s.stats.WOUAvg = s.wouSum / (s.stats.Requests - s.stats.Errors)
	if wou < 10*time.Microsecond {
		s.stats.WOUlt10us++
	} else if wou < 100*time.Microsecond {
		s.stats.WOUlt100us++
	} else if wou < 1000*time.Microsecond {
		s.stats.WOUlt1000us++
	} else {
		s.stats.WOUge1000us++
	}
	if wou < time.Millisecond {
		s.stats.WOUlt1ms++
	} else if wou < 10*time.Millisecond {
		s.stats.WOUlt10ms++
	} else if wou < 100*time.Millisecond {
		s.stats.WOUlt100ms++
	} else {
		s.stats.WOUge100ms++
	}
	if int64(wou) > s.stats.WOUMax {
		s.stats.WOUMax = int64(wou)
	}
}
