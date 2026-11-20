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

package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/facebook/time/cmd/ntripper/stats"
	"github.com/facebook/time/rtcm"
	"github.com/stretchr/testify/require"
)

func TestParseStationID(t *testing.T) {
	tests := map[string]uint16{
		"MOUNT01":             1,  // trailing "01" -> 1
		"STATION42":           42, // trailing digits
		"REF000":              0,  // trailing "000" -> 0
		"NoDigitsHere":        1,  // no trailing digits -> default
		"":                    1,  // empty -> default
		"4095":                1,  // all digits, no non-digit prefix -> default
		"CleanlyEvidentIbex1": 1,  // trailing "1"
	}
	for in, want := range tests {
		require.Equalf(t, want, parseStationID(in), "parseStationID(%q)", in)
	}
}

func TestIsWriteError(t *testing.T) {
	require.True(t, isWriteError(casterWrite("MSM7", errors.New("broken pipe"))))
	// Still detected when wrapped further upstream.
	require.True(t, isWriteError(fmt.Errorf("runOnce: %w", casterWrite("1033", os.ErrClosed))))
	require.False(t, isWriteError(errors.New("socket closed (EOF)")))
	require.False(t, isWriteError(fmt.Errorf("reading from socket: %w", os.ErrDeadlineExceeded)))
}

func TestSetMSMMultipleBit(t *testing.T) {
	obs := []rtcm.RawxObservation{
		{PrMes: 20000000, CpMes: 105000000, DoMes: -100, GnssID: rtcm.GnssGPS, SvID: 5, SigID: 0, CNO: 45, PrValid: true, CpValid: true},
	}
	frame, err := rtcm.EncodeMSM7(1, rtcm.GnssGPS, 100000, obs)
	require.NoError(t, err)

	// DF393 (multiple message bit) is payload bit 54; the encoder leaves it 0.
	require.Equal(t, uint32(0), multipleBit(frame))

	setMSMMultipleBit(frame)
	require.Equal(t, uint32(1), multipleBit(frame), "multiple-message bit set")

	// CRC must be recomputed and valid.
	pl := int(frame[1]&0x03)<<8 | int(frame[2])
	stored := uint32(frame[len(frame)-3])<<16 | uint32(frame[len(frame)-2])<<8 | uint32(frame[len(frame)-1])
	require.Equal(t, rtcm.CRC24Q(frame[:rtcm.HeaderSize+pl]), stored)
}

// multipleBit reads DF393 (payload bit 54: after msgnum(12)+staid(12)+epoch(30)).
func multipleBit(frame []byte) uint32 {
	r := rtcm.NewBitReader(frame[rtcm.HeaderSize:])
	r.Skip(54)
	return r.ReadBits(1)
}

func TestSetMSMMultipleBitShortFrameNoPanic(t *testing.T) {
	require.NotPanics(t, func() { setMSMMultipleBit([]byte{0xD3, 0x00, 0x05}) })
}

func TestSetupLogger(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "unknown"} {
		require.NotNil(t, setupLogger(lvl), "level %q", lvl)
	}
}

func TestParseStreamCleanFile(t *testing.T) {
	// A file of back-to-back valid RTCM3 frames parses without error.
	a, err := rtcm.Encode1033(rtcm.AntennaDescriptor{StationID: 1, AntennaType: "A"})
	require.NoError(t, err)
	b, err := rtcm.Encode1033(rtcm.AntennaDescriptor{StationID: 2, AntennaType: "BB"})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "stream.rtcm3")
	require.NoError(t, os.WriteFile(path, append(append([]byte{}, a...), b...), 0o600))
	require.NoError(t, parseStream(path))
}

func TestParseStreamMissingFile(t *testing.T) {
	require.Error(t, parseStream(filepath.Join(t.TempDir(), "does-not-exist")))
}

func TestTowToMs(t *testing.T) {
	tests := []struct {
		name   string
		rcvTow float64
		want   uint32
	}{
		{"ExactSecond", 230022.0, 230022000},
		{"FloatShortOfSecond", 230021.999999999, 230022000},
		{"FloatPastSecond", 230022.000000001, 230022000},
		{"GenuineMidSecond", 230022.009, 230022009},
		{"HalfMillisecondRoundsUp", 230022.0005, 230022001},
		{"WeekStart", 0.0, 0},
		{"WeekRollover", 604799.9999999, 0},
		{"LastMillisecondOfWeek", 604799.999, 604799999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, towToMs(tt.rcvTow))
		})
	}
}

// An epoch measured on the second must not be stamped into the previous one.
func TestTowToMsKeepsEpochOnTheSecond(t *testing.T) {
	for sec := 230020; sec < 230030; sec++ {
		got := towToMs(float64(sec))
		require.Zero(t, got%1000, "tow=%d stamped %d ms off the second", sec, got%1000)
	}
}

func TestDueNow(t *testing.T) {
	tests := []struct {
		name     string
		last     time.Time
		interval time.Duration
		want     bool
	}{
		{"IntervalElapsed", time.Now().Add(-2 * time.Second), time.Second, true},
		{"IntervalNotElapsed", time.Now(), time.Second, false},
		{"ZeroIntervalDisables", time.Now().Add(-time.Hour), 0, false},
		{"NegativeIntervalDisables", time.Now().Add(-time.Hour), -time.Second, false},
		{"ExactlyAtInterval", time.Now().Add(-time.Second), time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, dueNow(tt.last, tt.interval))
		})
	}
}

// A socket that does not exist is a wait condition: run must keep retrying
// rather than exiting the process.
func TestRunRetriesWhenSocketMissing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg := Config{
		socket:            filepath.Join(t.TempDir(), "absent.sock"),
		dryRun:            true,
		reconnectInterval: time.Millisecond,
	}
	st := stats.NewJSONStats()

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, cfg, setupLogger("error"), st)
	}()

	require.Eventually(t, func() bool {
		return st.Reconnects() > 1
	}, 5*time.Second, 10*time.Millisecond, "run must retry a missing socket, not exit")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "run did not return after context cancellation")
	}
}

type recordingWriter struct {
	writes [][]byte
	failAt int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte{}, p...))
	if w.failAt > 0 && len(w.writes) >= w.failAt {
		return 0, errors.New("caster gone")
	}
	return len(p), nil
}

// fakeEphCache returns n stub cached ephemeris frames.
type fakeEphCache struct{ n int }

func (f fakeEphCache) All() [][]byte {
	out := make([][]byte, 0, f.n)
	for i := range f.n {
		out = append(out, []byte{0xD3, 0x00, byte(i)})
	}
	return out
}

func TestPeriodicSenderCadences(t *testing.T) {
	const ephInterval = 8 * time.Second
	msg1033 := []byte{0xD3, 0x00, 0x01, 0xFF}

	tests := []struct {
		name        string
		lastMsg1033 time.Time
		lastEph     time.Time
		ephInterval time.Duration
		cachedEph   int
		wantWrites  int
		wantEphSent uint64
	}{
		{
			name:        "EphemerisDueOnly",
			lastMsg1033: time.Now(),
			lastEph:     time.Now().Add(-ephInterval),
			ephInterval: ephInterval,
			cachedEph:   3,
			wantWrites:  3,
			wantEphSent: 3,
		},
		{
			name:        "Msg1033DueOnly",
			lastMsg1033: time.Now().Add(-msg1033Interval),
			lastEph:     time.Now(),
			ephInterval: ephInterval,
			cachedEph:   3,
			wantWrites:  1,
			wantEphSent: 0,
		},
		{
			name:        "BothDue",
			lastMsg1033: time.Now().Add(-msg1033Interval),
			lastEph:     time.Now().Add(-ephInterval),
			ephInterval: ephInterval,
			cachedEph:   2,
			wantWrites:  3,
			wantEphSent: 2,
		},
		{
			name:        "NeitherDue",
			lastMsg1033: time.Now(),
			lastEph:     time.Now(),
			ephInterval: ephInterval,
			cachedEph:   2,
			wantWrites:  0,
			wantEphSent: 0,
		},
		{
			name:        "ZeroIntervalDisablesEphemeris",
			lastMsg1033: time.Now(),
			lastEph:     time.Now().Add(-time.Hour),
			ephInterval: 0,
			cachedEph:   2,
			wantWrites:  0,
			wantEphSent: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &periodicSender{
				lastMsg1033: tt.lastMsg1033,
				lastEph:     tt.lastEph,
				ephInterval: tt.ephInterval,
			}
			w := &recordingWriter{}
			sent, err := p.send(w, msg1033, fakeEphCache{tt.cachedEph})
			require.NoError(t, err)
			require.Equal(t, tt.wantEphSent, sent)
			require.Len(t, w.writes, tt.wantWrites)
		})
	}
}

// A due ephemeris tick must not advance the 1033 timer, and vice versa.
func TestPeriodicSenderTimersAdvanceIndependently(t *testing.T) {
	const ephInterval = 8 * time.Second
	before1033 := time.Now().Add(-time.Minute)
	p := &periodicSender{
		lastMsg1033: before1033,
		lastEph:     time.Now().Add(-ephInterval),
		ephInterval: ephInterval,
	}

	_, err := p.send(&recordingWriter{}, []byte{0xD3}, fakeEphCache{1})
	require.NoError(t, err)
	require.True(t, p.lastMsg1033.After(before1033), "1033 was due, so its timer advanced")

	sentEph := p.lastEph
	p.lastMsg1033 = time.Now().Add(-msg1033Interval)
	_, err = p.send(&recordingWriter{}, []byte{0xD3}, fakeEphCache{1})
	require.NoError(t, err)
	require.Equal(t, sentEph, p.lastEph, "1033 firing must not advance the ephemeris timer")
}

func TestPeriodicSenderEphemerisWriteError(t *testing.T) {
	p := &periodicSender{
		lastMsg1033: time.Now(),
		lastEph:     time.Now().Add(-time.Minute),
		ephInterval: time.Second,
	}
	w := &recordingWriter{failAt: 2}

	sent, err := p.send(w, []byte{0xD3}, fakeEphCache{3})
	require.Error(t, err)
	require.True(t, isWriteError(err), "ephemeris write failure must be a caster write error")
	require.Equal(t, uint64(1), sent, "frames written before the failure are counted")
}

// ubxFrame wraps a UBX-RXM payload in a frame with its Fletcher-8 checksum.
func ubxFrame(id byte, p []byte) []byte {
	f := []byte{rtcm.UBXSync1, rtcm.UBXSync2, rtcm.UBXClassRXM, id, byte(len(p) & 0xFF), byte(len(p) >> 8 & 0xFF)}
	f = append(f, p...)
	var ckA, ckB byte
	for _, c := range f[2:] {
		ckA += c
		ckB += ckA
	}
	return append(f, ckA, ckB)
}

// rawxFrame is a UBX-RXM-RAWX frame at GPS time of week tow with one GPS L1 measurement.
func rawxFrame(tow float64) []byte {
	return rawxFrameAtWeek(tow, 2400)
}

func rawxFrameAtWeek(tow float64, week uint16) []byte {
	p := make([]byte, 16+32)
	binary.LittleEndian.PutUint64(p[0:8], math.Float64bits(tow))
	binary.LittleEndian.PutUint16(p[8:10], week)
	p[10], p[11] = 18, 1
	m := p[16:]
	binary.LittleEndian.PutUint64(m[0:8], math.Float64bits(2.2e7))
	binary.LittleEndian.PutUint64(m[8:16], math.Float64bits(1.156e8))
	binary.LittleEndian.PutUint32(m[16:20], math.Float32bits(-1234.5))
	m[20], m[21] = rtcm.GnssGPS, 7
	binary.LittleEndian.PutUint16(m[24:26], 64500)
	m[26], m[30] = 44, 0x03
	return ubxFrame(rtcm.UBXMsgRAWX, p)
}

func descriptor(t *testing.T, antenna string) []byte {
	frame, err := rtcm.Encode1033(rtcm.AntennaDescriptor{StationID: 1, AntennaType: antenna})
	require.NoError(t, err)
	return frame
}

// serveSocket serves stream to the first client of a Unix socket, which is gone after that.
func serveSocket(t *testing.T, stream ...[]byte) string {
	path := filepath.Join(t.TempDir(), "rtcm.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		for _, b := range stream {
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	}()
	return path
}

// msmEpoch reads the MSM epoch time (DF004, payload bits 24-53).
func msmEpoch(frame []byte) uint32 {
	r := rtcm.NewBitReader(frame[rtcm.HeaderSize:])
	r.Skip(24)
	return r.ReadBits(30)
}

func TestStreamFramesSendsOneEpochPerInterval(t *testing.T) {
	tests := map[string]struct {
		interval time.Duration
		sent     []uint32
	}{
		"default zero sends every epoch": {0, []uint32{0, 1, 2, 3, 4}},
		"one second at 1 Hz":             {time.Second, []uint32{0, 1, 2, 3, 4}},
		"two seconds":                    {2 * time.Second, []uint32{0, 2, 4}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			stream := make([][]byte, 0, 10)
			for i := range 5 {
				stream = append(stream, rawxFrame(61218+float64(i)), descriptor(t, fmt.Sprintf("epoch%d", i)))
			}
			conn, err := net.Dial("unix", serveSocket(t, stream...))
			require.NoError(t, err)
			defer conn.Close()
			caster := &recordingWriter{}

			err = streamFrames(t.Context(), conn, caster, slog.New(slog.DiscardHandler), stats.NewJSONStats(),
				1, descriptor(t, "ADVNULLANTENNA"), rtcm.NewEphCollector(), time.Hour, newEpochGate(tt.interval))

			require.ErrorContains(t, err, "EOF")
			wantEpochs, epochs := make([]uint32, 0, len(tt.sent)), make([]uint32, 0, len(tt.sent))
			wantNative, native := make([][]byte, 0, len(tt.sent)), make([][]byte, 0, len(tt.sent))
			for _, i := range tt.sent {
				wantEpochs = append(wantEpochs, (61218+i)*1000)
				wantNative = append(wantNative, descriptor(t, fmt.Sprintf("epoch%d", i)))
			}
			for _, w := range caster.writes[1:] {
				f, err := rtcm.ParseFrame(w)
				require.NoError(t, err)
				if f.MessageType == rtcm.TypeGPSMSM7 {
					epochs = append(epochs, msmEpoch(w))
				} else {
					native = append(native, w)
				}
			}
			require.Equal(t, wantEpochs, epochs)
			require.Equal(t, wantNative, native, "the receiver's own frames follow their epoch")
		})
	}
}

func TestStreamFramesKeepsEpochGateAcrossSocketReconnects(t *testing.T) {
	caster := &recordingWriter{}
	epochsGate := newEpochGate(time.Minute)
	streams := [][][]byte{
		{rawxFrame(61200), descriptor(t, "first"), rawxFrame(61201), descriptor(t, "skipped")},
		{descriptor(t, "skipped after reconnect"), rawxFrame(61205), descriptor(t, "skipped again"),
			rawxFrame(61260), descriptor(t, "next interval")},
	}
	msg1033 := descriptor(t, "ADVNULLANTENNA")
	for _, stream := range streams {
		conn, err := net.Dial("unix", serveSocket(t, stream...))
		require.NoError(t, err)
		err = streamFrames(t.Context(), conn, caster, slog.New(slog.DiscardHandler), stats.NewJSONStats(),
			1, msg1033, rtcm.NewEphCollector(), time.Hour, epochsGate)
		conn.Close()
		require.ErrorContains(t, err, "EOF")
	}

	var epochs []uint32
	var native [][]byte
	for _, w := range caster.writes {
		f, err := rtcm.ParseFrame(w)
		require.NoError(t, err)
		if f.MessageType == rtcm.TypeGPSMSM7 {
			epochs = append(epochs, msmEpoch(w))
		} else if !bytes.Equal(w, msg1033) {
			native = append(native, w)
		}
	}
	require.Equal(t, []uint32{61200000, 61260000}, epochs)
	require.Equal(t, [][]byte{descriptor(t, "first"), descriptor(t, "next interval")}, native)
}

func TestStreamFramesForwardsSparseReceiverFrames(t *testing.T) {
	// Receiver frames only after epochs 1 and 5, neither of which is sent at a 2 s interval.
	stream := [][]byte{
		rawxFrame(61218), rawxFrame(61219), descriptor(t, "after 1"),
		rawxFrame(61220), rawxFrame(61221), rawxFrame(61222), rawxFrame(61223), descriptor(t, "after 5"),
	}
	conn, err := net.Dial("unix", serveSocket(t, stream...))
	require.NoError(t, err)
	defer conn.Close()
	caster := &recordingWriter{}
	msg1033 := descriptor(t, "ADVNULLANTENNA")

	err = streamFrames(t.Context(), conn, caster, slog.New(slog.DiscardHandler), stats.NewJSONStats(),
		1, msg1033, rtcm.NewEphCollector(), time.Hour, newEpochGate(2*time.Second))

	require.ErrorContains(t, err, "EOF")
	native := make([][]byte, 0, 2)
	for _, w := range caster.writes {
		f, err := rtcm.ParseFrame(w)
		require.NoError(t, err)
		if f.MessageType != rtcm.TypeGPSMSM7 && !bytes.Equal(w, msg1033) {
			native = append(native, w)
		}
	}
	require.Equal(t, [][]byte{descriptor(t, "after 1"), descriptor(t, "after 5")}, native,
		"each receiver frame type goes out once per interval, not only after a sent epoch")
}

func TestStreamFramesSamplesAcrossWeekRollover(t *testing.T) {
	conn, err := net.Dial("unix", serveSocket(t,
		rawxFrameAtWeek(604740, 2400), rawxFrameAtWeek(604799.9999999, 2400), rawxFrameAtWeek(1, 2401)))
	require.NoError(t, err)
	defer conn.Close()
	caster := &recordingWriter{}
	err = streamFrames(t.Context(), conn, caster, slog.New(slog.DiscardHandler), stats.NewJSONStats(),
		1, descriptor(t, "ADVNULLANTENNA"), rtcm.NewEphCollector(), time.Hour, newEpochGate(time.Minute))
	require.ErrorContains(t, err, "EOF")
	var epochs []uint32
	for _, w := range caster.writes {
		f, err := rtcm.ParseFrame(w)
		require.NoError(t, err)
		if f.MessageType == rtcm.TypeGPSMSM7 {
			epochs = append(epochs, msmEpoch(w))
		}
	}
	require.Equal(t, []uint32{604740000, 0}, epochs)
}

// gpsEphemeris returns the UBX-RXM-SFRBX frames of GPS LNAV subframes 1-3 for sv, IODE 0x42.
func gpsEphemeris(sv uint8) [][]byte {
	frames := make([][]byte, 0, 3)
	for id := range uint32(3) {
		sf := make([]byte, 30)
		setBits(sf, 24, 24, (id+1)<<2)
		switch id {
		case 0:
			setBits(sf, 70, 2, 1)
			setBits(sf, 168, 8, 0x42)
		case 1:
			setBits(sf, 48, 8, 0x42)
		case 2:
			setBits(sf, 216, 8, 0x42)
		}
		p := make([]byte, 8+40)
		p[0], p[1], p[4] = rtcm.GnssGPS, sv, 10
		for w := range 10 {
			word := uint32(sf[3*w])<<16 | uint32(sf[3*w+1])<<8 | uint32(sf[3*w+2])
			binary.LittleEndian.PutUint32(p[8+4*w:], word<<6)
		}
		frames = append(frames, ubxFrame(rtcm.UBXMsgSFRBX, p))
	}
	return frames
}

func setBits(buf []byte, pos, n int, v uint32) {
	for i := range n {
		if v>>(n-1-i)&1 == 1 {
			buf[(pos+i)/8] |= 1 << (7 - (pos+i)%8)
		}
	}
}

func TestStreamFramesKeepsPeriodicSendsOnSkippedEpochs(t *testing.T) {
	stream := append(gpsEphemeris(7), rawxFrame(61218), rawxFrame(61219), rawxFrame(61220))
	conn, err := net.Dial("unix", serveSocket(t, stream...))
	require.NoError(t, err)
	defer conn.Close()
	caster := &recordingWriter{}

	err = streamFrames(t.Context(), conn, caster, slog.New(slog.DiscardHandler), stats.NewJSONStats(),
		1, descriptor(t, "ADVNULLANTENNA"), rtcm.NewEphCollector(), time.Nanosecond, newEpochGate(time.Hour))

	require.ErrorContains(t, err, "EOF")
	types := make([]uint16, 0, len(caster.writes))
	for _, w := range caster.writes {
		f, err := rtcm.ParseFrame(w)
		require.NoError(t, err)
		types = append(types, f.MessageType)
	}
	require.Equal(t, []uint16{1033, 1019, 1019, rtcm.TypeGPSMSM7, 1019, 1019}, types,
		"the cached 1019 is re-sent before every scanned frame, sent epoch or not")
}
