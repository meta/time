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

/*
Package protocol implements ntp packet and basic functions to work with.
It provides quick and transparent translation between 48 bytes and
simply accessible struct in the most efficient way.
*/
package protocol

import (
	"time"
)

// NanosecondsToUnix is the difference between the start of NTP Era 0 and the Unix epoch in nanoseconds
// Jan-1 1900 00:00:00 UTC (start of NTP epoch Era 0) and Jan-1 1970 00:00:00 UTC (start of Unix epoch)
// Formula is 70 * (365 + 17) * 86400 (17 leap days)
const NanosecondsToUnix = int64(2_208_988_800_000_000_000)

// Time is converting Unix time to sec and frac NTP format
func Time(t time.Time) (seconds uint32, fractions uint32) {
	nsec := t.UnixNano() + NanosecondsToUnix
	sec := nsec / time.Second.Nanoseconds()
	return uint32(sec), uint32((nsec - sec*time.Second.Nanoseconds()) << 32 / time.Second.Nanoseconds())
}

// Unix is converting NTP seconds and fractions into Unix time.
// As described in RFC 4330 section 3, seconds with the most significant bit
// set are in NTP era 0 (1968-2036), and seconds with it unset are in era 1
// (2036-2104), since the 32-bit seconds field wraps on 2036-02-07.
func Unix(seconds, fractions uint32) time.Time {
	secs := int64(seconds) - NanosecondsToUnix/time.Second.Nanoseconds()
	if seconds&(1<<31) == 0 {
		secs += 1 << 32
	}
	// convert fractional to nanos, rounding to the nearest nanosecond so that Unix(Time(t)) == t
	nanos := (int64(fractions)*time.Second.Nanoseconds() + 1<<31) >> 32
	return time.Unix(secs, nanos)
}

// Offset uses NTP algorithm for clock offset
func Offset(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime time.Time) int64 {
	outboundClockDelta := serverReceiveTime.Sub(originTime).Nanoseconds()
	inboundClockDelta := serverTransmitTime.Sub(clientReceiveTime).Nanoseconds()

	return (outboundClockDelta + inboundClockDelta) / 2
}

// RoundTripDelay uses NTP algorithm for roundtrip network delay
func RoundTripDelay(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime time.Time) int64 {
	totalDelay := clientReceiveTime.Sub(originTime).Nanoseconds()
	serverDelay := serverTransmitTime.Sub(serverReceiveTime).Nanoseconds()

	return (totalDelay - serverDelay)
}

// CorrectTime returns the correct time based on computed offset
func CorrectTime(clientReceiveTime time.Time, offset int64) time.Time {
	correctTime := clientReceiveTime.Add(time.Duration(offset))

	return correctTime
}
