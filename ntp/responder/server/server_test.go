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

package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	ntp "github.com/facebook/time/ntp/protocol"
	"github.com/facebook/time/ntp/protocol/nts"
	"github.com/facebook/time/ntp/responder/checker"
	"github.com/facebook/time/ntp/responder/stats"
	"github.com/facebook/time/timestamp"
	"github.com/stretchr/testify/require"
)

var ts = time.Unix(1585231321, 148166539)

// Packet request. From ntpdate run
var ntpRequest = &ntp.Packet{
	Settings:       227,
	Poll:           3,
	Precision:      -6,
	RootDelay:      65536,
	RootDispersion: 65536,
	TxTimeSec:      3794210679,
	TxTimeFrac:     2718216404,
}

// try to listen on any port, if it fails - skip the test
func tryListenUDP(t *testing.T) *net.UDPConn {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Skipf("failed to listen on any port: %v", err)
		return nil
	}
	return conn
}

func TestFillStaticHeadersStratum(t *testing.T) {
	stratum := 1
	s := &Server{Config: Config{Stratum: stratum}}
	response := &ntp.Packet{}
	s.fillStaticHeaders(response)
	require.Equal(t, uint8(stratum), response.Stratum)
}

func TestFillStaticHeadersReferenceID(t *testing.T) {
	s := &Server{Config: Config{RefID: "CHANDLER"}}
	response := &ntp.Packet{}

	s.fillStaticHeaders(response)
	require.Equal(t, binary.BigEndian.Uint32([]byte("CHAN")), response.ReferenceID, "Reference-ID must be 4 bytes")
}

func TestFillStaticHeadersRootDelay(t *testing.T) {
	s := &Server{}
	response := &ntp.Packet{}

	s.fillStaticHeaders(response)
	require.Equal(t, uint32(0), response.RootDelay, "Root delay should be 0 if stratum is 1")
}

func TestFillStaticHeadersRootDispersion(t *testing.T) {
	s := &Server{}
	response := &ntp.Packet{}

	s.fillStaticHeaders(response)
	require.Equal(t, uint32(1), response.RootDispersion, "Root dispersion should be 0.000015")
}

func TestGenerateResponsePoll(t *testing.T) {
	request := &ntp.Packet{Poll: 8}
	response := &ntp.Packet{}
	generateResponse(ts, ts, request, response)
	require.Equal(t, request.Poll, response.Poll)
}

func TestGenerateResponsetss(t *testing.T) {
	request := &ntp.Packet{TxTimeSec: 3794210679, TxTimeFrac: 2718216404}
	response := &ntp.Packet{}
	nowSec, nowFrac := ntp.Time(ts)

	generateResponse(ts, ts, request, response)

	// Reference ts must to the closest /1000s
	lastSync := time.Unix(ts.Unix()/1000*1000, 0)
	lastSyncSec, lastSyncFrac := ntp.Time(lastSync)
	require.Equal(t, lastSyncSec, response.RefTimeSec)
	require.Equal(t, lastSyncFrac, response.RefTimeFrac)

	// Originate ts must be the same
	require.Equal(t, request.TxTimeSec, response.OrigTimeSec)
	require.Equal(t, request.TxTimeFrac, response.OrigTimeFrac)

	// Receive ts must be current ts
	require.Equal(t, nowSec, response.RxTimeSec)
	require.Equal(t, nowFrac, response.RxTimeFrac)

	// Transmit ts must be current ts
	require.Equal(t, nowSec, response.TxTimeSec)
	require.Equal(t, nowFrac, response.TxTimeFrac)
}

func TestListener(t *testing.T) {
	s := &Server{
		Checker: &checker.SimpleChecker{
			ExpectedListeners: 1,
			ExpectedWorkers:   0,
		},
		Config: Config{Workers: 42, TimestampType: timestamp.SWRX, Iface: "lo"},
	}
	conn := tryListenUDP(t)
	defer conn.Close()
	go s.startListener(conn)
	time.Sleep(100 * time.Millisecond)

	err := s.Checker.Check()
	require.NoError(t, err)
}

func TestWorker(t *testing.T) {
	s := &Server{
		Checker: &checker.SimpleChecker{
			ExpectedListeners: 0,
			ExpectedWorkers:   1,
		},
		Stats: &stats.JSONStats{},
		tasks: make(chan task),
	}

	// listen to incoming udp ntp.
	conn := tryListenUDP(t)
	defer conn.Close()

	// get connection file descriptor
	connFd, err := timestamp.ConnFd(conn)
	require.NoError(t, err)

	sa := netip.MustParseAddrPort("127.0.0.1:0")

	go s.startWorker()
	time.Sleep(100 * time.Millisecond)
	err = s.Checker.Check()
	require.NoError(t, err)
	s.tasks <- task{connFd: connFd, addr: sa, received: time.Now(), request: ntpRequest, stats: &stats.JSONStats{}}
}

func TestServer(t *testing.T) {
	workers := 5
	requests := 1000
	s := &Server{
		Checker: &checker.SimpleChecker{
			ExpectedListeners: 1,
			ExpectedWorkers:   int64(workers),
		},
		Stats:  &stats.JSONStats{},
		tasks:  make(chan task, workers),
		Config: Config{Workers: workers, TimestampType: timestamp.SWRX, Iface: "lo"},
	}
	// create workers
	for range workers {
		go s.startWorker()
	}
	conn := tryListenUDP(t)
	defer conn.Close()

	// increase socket buffer sizes to 1MB to handle high volume
	err := conn.SetReadBuffer(1024 * 1024)
	require.NoError(t, err)
	err = conn.SetWriteBuffer(1024 * 1024)
	require.NoError(t, err)

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	go s.startListener(conn)

	time.Sleep(100 * time.Millisecond)

	err = s.Checker.Check()
	require.NoError(t, err)

	// talk to local server
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", localAddr.Port))
	sendConn, err := net.DialTimeout("udp", addr, time.Second)
	require.Nil(t, err)
	defer sendConn.Close()

	// set read deadline to prevent test from hanging forever
	err = sendConn.SetDeadline(time.Now().Add(10 * time.Second))
	require.NoError(t, err)

	// send requests to server, check if response makes sense
	for range requests {
		clientTransmitTime := time.Now()
		sec, frac := ntp.Time(clientTransmitTime)

		request := &ntp.Packet{
			Settings:   0x1B,
			TxTimeSec:  sec,
			TxTimeFrac: frac,
		}
		response := &ntp.Packet{}

		reqBytes, err := request.Bytes()
		require.Nil(t, err, "sending request should not err")
		_, err = sendConn.Write(reqBytes)
		require.Nil(t, err, "sending request should not err")
		respBuf := make([]byte, ntp.PacketSizeBytes)
		_, err = sendConn.Read(respBuf)
		require.Nil(t, err, "receiving response should not err")
		err = response.UnmarshalBinary(respBuf)
		require.Nil(t, err, "receiving response should not err")
		require.Equal(t, sec, response.OrigTimeSec, "response Origin Time seconds should match our TX seconds")
		require.Equal(t, frac, response.OrigTimeFrac, "response Origin Time fraction should match our TX fraction")
	}
	err = s.Checker.Check()
	require.NoError(t, err)
}

func Benchmark_generateResponse(b *testing.B) {
	for range b.N {
		request := &ntp.Packet{}
		response := &ntp.Packet{}
		generateResponse(ts, ts, request, response)
	}
}

func Benchmark_fillStaticHeaders(b *testing.B) {
	s := &Server{}
	for range b.N {
		response := &ntp.Packet{}
		s.fillStaticHeaders(response)
	}
}

func TestServerNTS(t *testing.T) {
	ks := newTestKeystore(t) // reuse helper from nts_test.go
	s := &Server{
		Checker: &checker.SimpleChecker{ExpectedListeners: 1, ExpectedWorkers: 1},
		Stats:   &stats.JSONStats{},
		tasks:   make(chan task, 1),
		Config:  Config{Workers: 1, TimestampType: timestamp.SWRX, Iface: "lo", Keystore: ks},
	}
	go s.startWorker()

	conn := tryListenUDP(t)
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	go s.startListener(conn)
	time.Sleep(100 * time.Millisecond)

	// build a real NTS request (helper already in nts_test.go)
	uid := randBytes(t, nts.MinUniqueIdentifierLen)
	req, s2c := buildNTSRequest(t, ks, ntp.AEADAESSIVCMAC512, uid, 2)

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", localAddr.Port))
	sendConn, err := net.DialTimeout("udp", addr, time.Second)
	require.NoError(t, err)
	defer sendConn.Close()

	_, err = sendConn.Write(req)
	require.NoError(t, err)

	buf := make([]byte, maxPacketSizeBytes)
	require.NoError(t, sendConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, err := sendConn.Read(buf)
	require.NoError(t, err)

	// open the response as the client would — proves the whole wiring works
	inner, cleartext := openResponse(t, buf[:n], ntp.AEADAESSIVCMAC512, s2c)
	require.Equal(t, 3, inner) // 1 + 2 placeholders
	require.Zero(t, cleartext)
}

// TestServerNTSAnswersPlainRequestWithLegacyMAC pins the outage the MAC split
// exists to end, at the layer where it was seen. A keystore is what makes the
// difference: startListener hands the whole datagram to UnmarshalBinary only when
// one is set, so on an NTS-enabled responder an RFC 5905 MAC used to fail the
// parse and the client got nothing back. Without the split in
// ntp.Packet.UnmarshalBinary this test times out on the read.
func TestServerNTSAnswersPlainRequestWithLegacyMAC(t *testing.T) {
	s := &Server{
		Checker: &checker.SimpleChecker{ExpectedListeners: 1, ExpectedWorkers: 1},
		Stats:   &stats.JSONStats{},
		tasks:   make(chan task, 1),
		Config: Config{
			Workers: 1, TimestampType: timestamp.SWRX, Iface: "lo", Keystore: newTestKeystore(t),
		},
	}
	go s.startWorker()

	conn := tryListenUDP(t)
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	go s.startListener(conn)
	time.Sleep(100 * time.Millisecond)

	header, err := ntpRequest.Bytes()
	require.NoError(t, err)
	// Key identifier 1 plus an MD5 digest: 20 octets, and no extension-field
	// parser can read the identifier as a {type, length}.
	req := append(header, append([]byte{0x00, 0x00, 0x00, 0x01}, bytes.Repeat([]byte{0xAB}, 16)...)...)

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", localAddr.Port))
	sendConn, err := net.DialTimeout("udp", addr, time.Second)
	require.NoError(t, err)
	defer sendConn.Close()

	_, err = sendConn.Write(req)
	require.NoError(t, err)

	buf := make([]byte, maxPacketSizeBytes)
	require.NoError(t, sendConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, err := sendConn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, ntp.PacketSizeBytes, n)

	var reply ntp.Packet
	require.NoError(t, reply.UnmarshalBinary(buf[:n]))
	require.Equal(t, uint8(4), reply.Settings&0x07, "server mode")
	require.Empty(t, reply.ExtensionFields)
	require.Nil(t, reply.MAC, "we hold no symmetric key, so the reply carries no MAC")
}

// TestServerAnswersTrailersThatFrameAsFields covers the rest of the same outage.
// Splitting the MAC only rescues a trailer the field parser chokes on; when the
// trailer frames cleanly, splitTrailer returns fields and a nil MAC. serve then
// routed every parsed field into processNTSRequest, which rejects anything that
// is not a full NTS request, so these clients still got silence. A key
// identifier whose low 16 bits are a valid field length does exactly that, and
// so does any ordinary non-NTS field. The last case is the guard: a request that
// does name NTS must never be answered in plain NTP.
func TestServerAnswersTrailersThatFrameAsFields(t *testing.T) {
	const uidLen = ntp.ExtensionHeaderSize + nts.MinUniqueIdentifierLen
	uid := make([]byte, uidLen)
	binary.BigEndian.PutUint16(uid[0:2], uint16(ntp.UniqueIdentifier))
	binary.BigEndian.PutUint16(uid[2:4], uidLen)

	tests := []struct {
		name     string
		trailer  []byte
		answered bool
	}{
		{"CryptoNAKKeyID", []byte{0x00, 0x00, 0x00, 0x04}, true},
		{"MD5LengthKeyID", append([]byte{0x00, 0x00, 0x00, 0x14}, bytes.Repeat([]byte{0xAB}, 16)...), true},
		{"SHA1LengthKeyID", append([]byte{0x00, 0x00, 0x00, 0x18}, bytes.Repeat([]byte{0xAB}, 20)...), true},
		{"NonNTSExtensionField", []byte{0x05, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00}, true},
		{"UniqueIdentifierWithoutCookie", uid, false},
	}

	s := &Server{
		Checker: &checker.SimpleChecker{ExpectedListeners: 1, ExpectedWorkers: 1},
		Stats:   &stats.JSONStats{},
		tasks:   make(chan task, 1),
		Config: Config{
			Workers: 1, TimestampType: timestamp.SWRX, Iface: "lo", Keystore: newTestKeystore(t),
		},
	}
	go s.startWorker()

	conn := tryListenUDP(t)
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	go s.startListener(conn)
	time.Sleep(100 * time.Millisecond)

	header, err := ntpRequest.Bytes()
	require.NoError(t, err)
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", localAddr.Port))

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sendConn, err := net.DialTimeout("udp", addr, time.Second)
			require.NoError(t, err)
			defer sendConn.Close()

			_, err = sendConn.Write(append(bytes.Clone(header), tc.trailer...))
			require.NoError(t, err)

			buf := make([]byte, maxPacketSizeBytes)
			require.NoError(t, sendConn.SetReadDeadline(time.Now().Add(2*time.Second)))
			n, err := sendConn.Read(buf)
			if !tc.answered {
				require.Error(t, err, "a request naming NTS must not get a plain reply")
				return
			}
			require.NoError(t, err)
			require.Equal(t, ntp.PacketSizeBytes, n)

			var reply ntp.Packet
			require.NoError(t, reply.UnmarshalBinary(buf[:n]))
			require.Equal(t, uint8(4), reply.Settings&0x07, "server mode")
			require.Empty(t, reply.ExtensionFields)
			require.Nil(t, reply.MAC)
		})
	}
}

// errAnnounceStopInProgress is what FBAnnounce.Advertise returns once Withdraw
// has latched, verbatim from the production log that motivated this change.
var errAnnounceStopInProgress = errors.New("[announce]: stop is in progress. Do not announce")

type countingAnnounce struct {
	err              error
	stopBeginsDuring *Server
	// observes, when set, records whether that Server had already latched
	// stopping by the time Withdraw ran. Withdraw blocks for 60s of BGP
	// convergence, so latching after it would reopen the window fixed here.
	observes *Server
	// advertisedSignal, when set, reports each Advertise. advertised is written
	// only by the announce loop and withdrawn only by whoever calls Stop, so
	// neither is safe to read until Start has returned.
	advertisedSignal   chan struct{}
	advertised         int
	withdrawn          int
	stoppingAtWithdraw bool
}

func (a *countingAnnounce) Advertise([]net.IP) error {
	a.advertised++
	if a.stopBeginsDuring != nil {
		a.stopBeginsDuring.stopping.Store(true)
	}
	select {
	case a.advertisedSignal <- struct{}{}:
	default:
	}
	return a.err
}

func (a *countingAnnounce) Withdraw() error {
	a.withdrawn++
	if a.observes != nil {
		a.stoppingAtWithdraw = a.observes.stopping.Load()
	}
	return nil
}

type countingAnnounceStats struct {
	*stats.JSONStats
	set   int
	reset int
}

func (s *countingAnnounceStats) SetAnnounce()   { s.set++ }
func (s *countingAnnounceStats) ResetAnnounce() { s.reset++ }

func newAnnounceServer(a Announce) (*Server, *countingAnnounceStats) {
	st := &countingAnnounceStats{JSONStats: &stats.JSONStats{}}
	return &Server{Announce: a, Stats: st, Config: Config{ShouldAnnounce: true}}, st
}

func TestRefreshAnnounceStopsOnceStopBegins(t *testing.T) {
	a := &countingAnnounce{}
	s, st := newAnnounceServer(a)
	a.observes = s

	require.True(t, s.refreshAnnounce())
	require.Equal(t, 1, a.advertised)
	require.Equal(t, 1, st.set)

	s.Stop()
	require.Equal(t, 1, a.withdrawn)
	require.True(t, a.stoppingAtWithdraw, "Stop must latch stopping before Withdraw waits out BGP convergence")

	// Production Advertise refuses from here on, so even reaching it must leave
	// the gauge alone.
	a.err = errAnnounceStopInProgress

	require.False(t, s.refreshAnnounce(), "refresh must end the loop once Stop has begun")
	require.Equal(t, 1, a.advertised, "must not re-advertise against a withdrawal in flight")
	require.Zero(t, st.reset, "a deliberate stop is not an announcement failure")
}

func TestRefreshAnnounceReportsRealFailure(t *testing.T) {
	a := &countingAnnounce{err: errors.New("bgp session down")}
	s, st := newAnnounceServer(a)

	require.True(t, s.refreshAnnounce())
	require.Equal(t, 1, a.advertised)
	require.Zero(t, st.set)
	require.Equal(t, 1, st.reset)
}

func TestRefreshAnnounceHoldsGaugeDownWhenNotAnnouncing(t *testing.T) {
	a := &countingAnnounce{}
	st := &countingAnnounceStats{JSONStats: &stats.JSONStats{}}
	s := &Server{Announce: a, Stats: st}

	require.True(t, s.refreshAnnounce())
	require.Zero(t, a.advertised, "a responder started without -announce must not advertise")
	require.Zero(t, st.set)
	require.Equal(t, 1, st.reset, "the gauge must read 0, not go unpublished")
}

func TestStartEndsAnnounceLoopOnceStopBegins(t *testing.T) {
	prevInterval := announceInterval
	announceInterval = time.Millisecond

	a := &countingAnnounce{advertisedSignal: make(chan struct{}, 1)}
	s, st := newAnnounceServer(a)
	s.Checker = &checker.SimpleChecker{}

	ctx, cancel := context.WithCancel(t.Context())

	returned := make(chan struct{})
	go func() {
		s.Start(ctx, cancel)
		close(returned)
	}()
	// The loop reads announceInterval, so restoring it before the loop has
	// returned is a race on every t.Fatal path below.
	t.Cleanup(func() {
		cancel()
		<-returned
		announceInterval = prevInterval
	})

	select {
	case <-a.advertisedSignal:
	case <-time.After(10 * time.Second):
		t.Fatal("announce loop never advertised")
	}

	s.Stop()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Start must end the announce loop once Stop has begun")
	}
	require.Zero(t, st.reset, "a deliberate stop must not publish an announce failure")
}

func TestRefreshAnnounceIgnoresStopThatLandsDuringAdvertise(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "Advertise refuses", err: errors.New("[announce]: stop is in progress. Do not announce")},
		{name: "Advertise wins the race", err: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := &countingAnnounce{err: tt.err}
			s, st := newAnnounceServer(a)
			a.stopBeginsDuring = s

			require.False(t, s.refreshAnnounce(), "refresh must end the loop when Stop lands during Advertise")
			require.Equal(t, 1, a.advertised)
			require.Zero(t, st.reset, "a stop that lands during Advertise is not an announcement failure")
			require.Zero(t, st.set, "an advertisement that raced the withdrawal is not a success")
		})
	}
}
