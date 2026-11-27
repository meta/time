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
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ptp "github.com/facebook/time/ptp/protocol"
	"github.com/facebook/time/ptp/ptp4u/stats"
	"github.com/facebook/time/timestamp"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestFindWorker(t *testing.T) {
	c := &Config{
		clockIdentity: ptp.ClockIdentity(1234),
		StaticConfig: StaticConfig{
			TimestampType: timestamp.SW,
			SendWorkers:   10,
		},
	}
	s := Server{
		Config: c,
		Stats:  stats.NewJSONStats(1),
		sw:     make([]*sendWorker, c.SendWorkers),
	}

	for i := 0; i < s.Config.SendWorkers; i++ {
		s.sw[i] = newSendWorker(i, c, s.Stats)
	}

	clipi1 := ptp.PortIdentity{
		PortNumber:    1,
		ClockIdentity: ptp.ClockIdentity(1234),
	}

	clipi2 := ptp.PortIdentity{
		PortNumber:    2,
		ClockIdentity: ptp.ClockIdentity(1234),
	}

	clipi3 := ptp.PortIdentity{
		PortNumber:    1,
		ClockIdentity: ptp.ClockIdentity(5678),
	}

	// Consistent across multiple calls
	require.Equal(t, 3, s.findWorker(clipi1, 0).id)
	require.Equal(t, 3, s.findWorker(clipi1, 0).id)
	require.Equal(t, 3, s.findWorker(clipi1, 0).id)

	require.Equal(t, 7, s.findWorker(clipi2, 0).id)
	require.Equal(t, 6, s.findWorker(clipi3, 0).id)
}

func TestFindWorkerOffsetRotates(t *testing.T) {
	c := &Config{StaticConfig: StaticConfig{TimestampType: timestamp.SW, SendWorkers: 10}}
	s := Server{Config: c, Stats: stats.NewJSONStats(1), sw: make([]*sendWorker, c.SendWorkers)}
	for i := range c.SendWorkers {
		s.sw[i] = newSendWorker(i, c, s.Stats)
	}
	clientID := ptp.PortIdentity{PortNumber: 1, ClockIdentity: ptp.ClockIdentity(1234)}
	home := s.findWorker(clientID, 0).id
	for offset := range c.SendWorkers + 1 {
		require.Equal(t, (home+offset)%c.SendWorkers, s.findWorker(clientID, uint64(offset)).id)
	}
}

func TestDelayReqPortMoveReplacesSubscription(t *testing.T) {
	s, serverAddr := eventServer(t)
	clientFD := loopbackSocket(t)
	defer unix.Close(clientFD)

	client := ptp.PortIdentity{ClockIdentity: 5678, PortNumber: 1}
	home := s.findWorker(client, 0)
	moved := uint16(1)
	for s.findWorker(client, uint64(moved)) == home {
		moved++
	}
	sender := s.findWorker(client, uint64(moved))

	// resend until acted on: before the kernel turns RX stamping on, packets arrive unstamped and are dropped
	stay := delayReq(t, client, 0)
	var first *SubscriptionClient
	require.Eventually(t, func() bool {
		if first = home.FindSubscription(client, ptp.MessageDelayReq); first == nil {
			_ = unix.Sendto(clientFD, stay, 0, serverAddr)
		}
		return first != nil && first.Running()
	}, time.Second, 100*time.Millisecond)
	require.True(t, first.queue == home.queue)

	move := delayReq(t, client, moved)
	var second *SubscriptionClient
	require.Eventually(t, func() bool {
		if second = home.FindSubscription(client, ptp.MessageDelayReq); second == first {
			_ = unix.Sendto(clientFD, move, 0, serverAddr)
		}
		return second != nil && second != first && second.Running()
	}, time.Second, 100*time.Millisecond, "a port move must replace the subscription stored under the client's identity")
	require.True(t, second.queue == sender.queue, "the new subscription must send from the worker the offset picks")
	require.Eventually(t, func() bool { return !first.Running() }, time.Second, time.Millisecond,
		"the replaced subscription must stop")

	var subs int
	for _, w := range s.sw {
		subs += w.inventoryClients()
	}
	require.Equal(t, 1, subs)
}

func TestDelayReqPtpingRefreshFollowsPort(t *testing.T) {
	s, serverAddr := eventServer(t)
	client := ptp.PortIdentity{ClockIdentity: 5678, PortNumber: 4242}
	home := s.findWorker(client, 0)
	req := delayReq(t, client, 0)

	firstFD := loopbackSocket(t)
	defer unix.Close(firstFD)
	var sc *SubscriptionClient
	require.Eventually(t, func() bool {
		if sc = home.FindSubscription(client, ptp.MessageDelayReq); sc == nil {
			_ = unix.Sendto(firstFD, req, 0, serverAddr)
		}
		return sc != nil && sc.Running()
	}, time.Second, 100*time.Millisecond)

	movedFD := loopbackSocket(t)
	defer unix.Close(movedFD)
	movedSA, err := unix.Getsockname(movedFD)
	require.NoError(t, err)
	moved := timestamp.SockaddrToAddrPort(movedSA)
	require.Eventually(t, func() bool {
		if _, gaddr := sc.Addrs(); gaddr == moved {
			return true
		}
		_ = unix.Sendto(movedFD, req, 0, serverAddr)
		return false
	}, time.Second, 100*time.Millisecond, "a ptping refresh must send announce to the new ephemeral port")
	eaddr, _ := sc.Addrs()
	require.Equal(t, moved, eaddr)
	require.True(t, home.FindSubscription(client, ptp.MessageDelayReq) == sc, "a refresh must keep the subscription")
}

// eventServerDelayReq returns a client socket whose DELAY_REQ req to serverAddr the server queues on queue
func eventServerDelayReq(tb testing.TB) (clientFD int, req []byte, serverAddr unix.Sockaddr, queue chan *SubscriptionClient) {
	tb.Helper()
	s, serverAddr := eventServer(tb)
	clientFD = loopbackSocket(tb)
	tb.Cleanup(func() { unix.Close(clientFD) })
	client := ptp.PortIdentity{ClockIdentity: 5678, PortNumber: 1}
	queue = s.findWorker(client, 0).queue
	req = delayReq(tb, client, 0)

	// resend until acted on: before the kernel turns RX stamping on, packets arrive unstamped and are dropped
	require.Eventually(tb, func() bool {
		_ = unix.Sendto(clientFD, req, 0, serverAddr)
		select {
		case <-queue:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	}, time.Second, 20*time.Millisecond)
	return clientFD, req, serverAddr, queue
}

func BenchmarkHandleEventMessagesDelayReq(b *testing.B) {
	clientFD, req, serverAddr, queue := eventServerDelayReq(b)

	b.ReportAllocs()
	for b.Loop() {
		_ = unix.Sendto(clientFD, req, 0, serverAddr)
		<-queue
	}
}

func eventServer(t testing.TB) (*Server, unix.Sockaddr) {
	c := &Config{
		clockIdentity: ptp.ClockIdentity(1234),
		StaticConfig:  StaticConfig{TimestampType: timestamp.SW, SendWorkers: 10, QueueSize: 10},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	s := &Server{Config: c, Stats: stats.NewJSONStats(1), sw: make([]*sendWorker, c.SendWorkers), ctx: ctx, cancel: cancel}
	for i := range s.sw {
		s.sw[i] = newSendWorker(i, c, s.Stats)
	}

	// left open: closing it would spin handleEventMessages on read errors
	eFD := loopbackSocket(t)
	require.NoError(t, timestamp.EnableSWTimestampsRx(eFD))
	serverAddr, err := unix.Getsockname(eFD)
	require.NoError(t, err)
	go s.handleEventMessages(eFD, 0)
	return s, serverAddr
}

func loopbackSocket(t testing.TB) int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, timestamp.IPToSockaddr(net.ParseIP("127.0.0.1"), 0)))
	return fd
}

func delayReq(t testing.TB, client ptp.PortIdentity, offset uint16) []byte {
	size := binary.Size(ptp.Header{}) + binary.Size(ptp.SyncDelayReqBody{}) + binary.Size(ptp.AlternateResponsePortTLV{})
	req := &ptp.SyncDelayReq{
		Header: ptp.Header{
			SdoIDAndMsgType:    ptp.NewSdoIDAndMsgType(ptp.MessageDelayReq, 0),
			Version:            ptp.Version,
			MessageLength:      uint16(size), //#nosec G115
			FlagField:          ptp.FlagUnicast | ptp.FlagProfileSpecific1,
			SourcePortIdentity: client,
			LogMessageInterval: 0x7f,
		},
		TLVs: []ptp.TLV{&ptp.AlternateResponsePortTLV{
			TLVHead: ptp.TLVHead{
				TLVType:     ptp.TLVAlternateResponsePort,
				LengthField: uint16(binary.Size(ptp.AlternateResponsePortTLV{}) - binary.Size(ptp.TLVHead{})), //#nosec G115
			},
			Offset: offset,
		}},
	}
	buf := make([]byte, size+ptp.TrailingBytes)
	n, err := ptp.BytesTo(req, buf)
	require.NoError(t, err)
	return buf[:n]
}

func TestStartEventListener(t *testing.T) {
	ptp.PortEvent = 0
	c := &Config{
		clockIdentity: ptp.ClockIdentity(1234),
		StaticConfig: StaticConfig{
			TimestampType: timestamp.SW,
			SendWorkers:   10,
			RecvWorkers:   10,
			IP:            net.ParseIP("127.0.0.1"),
			Interface:     "lo",
		},
	}
	s := Server{
		Config: c,
		Stats:  stats.NewJSONStats(1),
		sw:     make([]*sendWorker, c.SendWorkers),
	}
	go s.startEventListener()
	time.Sleep(100 * time.Millisecond)
}

func TestStartAttemptsTXTS(t *testing.T) {
	orig := timestamp.AttemptsTXTS
	defer func() { timestamp.AttemptsTXTS = orig }()

	s := Server{Config: &Config{}}
	require.ErrorContains(t, s.Start(), "TX timestamp attempts must be positive")
	require.Equal(t, orig, timestamp.AttemptsTXTS)

	s.Config.AttemptsTXTS = 50
	s.Config.PidFile = filepath.Join(t.TempDir(), "ptp4u.pid")
	s.Config.Interface = "nonexistent0"
	require.ErrorContains(t, s.Start(), "unable to get mac address")
	require.Equal(t, 50, timestamp.AttemptsTXTS)
}

func TestStartGeneralListener(t *testing.T) {
	ptp.PortGeneral = 0
	c := &Config{
		clockIdentity: ptp.ClockIdentity(1234),
		StaticConfig: StaticConfig{
			TimestampType: timestamp.SW,
			SendWorkers:   10,
			RecvWorkers:   10,
			IP:            net.ParseIP("127.0.0.1"),
		},
	}
	s := Server{
		Config: c,
		Stats:  stats.NewJSONStats(1),
		sw:     make([]*sendWorker, c.SendWorkers),
	}
	go s.startGeneralListener()
	time.Sleep(100 * time.Millisecond)
}

func TestDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := Server{
		Stats:  stats.NewJSONStats(1),
		ctx:    ctx,
		cancel: cancel,
	}

	require.NoError(t, s.ctx.Err())
	s.Drain()
	require.ErrorIs(t, context.Canceled, s.ctx.Err())
}

func TestUndrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := Server{
		Stats:  stats.NewJSONStats(1),
		ctx:    ctx,
		cancel: cancel,
	}

	s.Drain()
	require.ErrorIs(t, context.Canceled, s.ctx.Err())
	s.Undrain()
	require.NoError(t, s.ctx.Err())
}

func TestDrainCountsSubscriptionsUnderWorkerLock(t *testing.T) {
	c := &Config{
		StaticConfig: StaticConfig{SendWorkers: 1},
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := Server{
		Config: c,
		Stats:  stats.NewJSONStats(c.SendWorkers),
		sw:     make([]*sendWorker, c.SendWorkers),
		ctx:    ctx,
		cancel: cancel,
	}
	for i := range s.sw {
		s.sw[i] = newSendWorker(i, c, s.Stats)
	}
	types := []ptp.MessageType{ptp.MessageAnnounce, ptp.MessageSync}
	held := make([]*SubscriptionClient, len(types))
	for i, st := range types {
		held[i] = &SubscriptionClient{running: true}
		s.sw[0].RegisterSubscription(ptp.PortIdentity{PortNumber: 1}, st, held[i])
	}
	require.Equal(t, len(held), s.sw[0].inventoryClients())

	registering := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		close(registering)
		for msgType := uint8(1); msgType != 0; msgType++ {
			time.Sleep(2 * time.Millisecond)
			s.sw[0].RegisterSubscription(
				ptp.PortIdentity{PortNumber: 2},
				ptp.MessageType(msgType),
				&SubscriptionClient{},
			)
		}
		for _, sc := range held {
			sc.setRunning(false)
		}
	})

	<-registering
	s.Drain()
	wg.Wait()

	require.ErrorIs(t, s.ctx.Err(), context.Canceled)
	s.sw[0].mux.Lock()
	var remaining int
	for _, subs := range s.sw[0].clients {
		remaining += len(subs)
	}
	s.sw[0].mux.Unlock()
	require.Zero(t, remaining)
}

func TestHandleSighup(t *testing.T) {
	expected := &Config{
		DynamicConfig: DynamicConfig{
			ClockAccuracy:  0,
			ClockClass:     1,
			DrainInterval:  2 * time.Second,
			MaxSubDuration: 3 * time.Hour,
			MetricInterval: 4 * time.Minute,
			MinSubInterval: 5 * time.Second,
			UTCOffset:      37 * time.Second,
		},
	}

	c := &Config{
		StaticConfig: StaticConfig{
			SendWorkers: 2,
			QueueSize:   10,
		},
	}
	s := Server{
		Config: c,
		Stats:  stats.NewJSONStats(1),
		sw:     make([]*sendWorker, c.SendWorkers),
	}
	clipi := ptp.PortIdentity{
		PortNumber:    1,
		ClockIdentity: ptp.ClockIdentity(1234),
	}

	s.sw[0] = newSendWorker(0, s.Config, s.Stats)
	s.sw[1] = newSendWorker(0, s.Config, s.Stats)
	sa := netip.MustParseAddrPort("127.0.0.1:123")
	scA := NewSubscriptionClient(s.sw[0].queue, s.sw[0].signalingQueue, sa, sa, ptp.MessageAnnounce, c, time.Second, time.Now().Add(time.Minute))
	scS := NewSubscriptionClient(s.sw[1].queue, s.sw[1].signalingQueue, sa, sa, ptp.MessageSync, c, time.Second, time.Now().Add(time.Minute))
	s.sw[0].RegisterSubscription(clipi, ptp.MessageAnnounce, scA)
	s.sw[1].RegisterSubscription(clipi, ptp.MessageSync, scS)

	go scA.Start(context.Background())
	go scS.Start(context.Background())
	time.Sleep(100 * time.Millisecond)

	require.Equal(t, 1, len(s.sw[0].queue))
	require.Equal(t, 1, len(s.sw[1].queue))

	cfg, err := os.CreateTemp("", "ptp4u")
	require.NoError(t, err)
	defer os.Remove(cfg.Name())

	config := `clockaccuracy: 0
clockclass: 1
draininterval: "2s"
maxsubduration: "3h"
metricinterval: "4m"
minsubinterval: "5s"
utcoffset: "37s"
`
	_, err = cfg.WriteString(config)
	require.NoError(t, err)

	c.ConfigFile = cfg.Name()

	go s.handleSighup()
	time.Sleep(100 * time.Millisecond)

	err = unix.Kill(unix.Getpid(), unix.SIGHUP)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	dcMux.Lock()
	require.Equal(t, expected.DynamicConfig, c.DynamicConfig)
	dcMux.Unlock()

	require.Equal(t, 1, len(s.sw[0].queue))
	require.Equal(t, 1, len(s.sw[1].queue))
}

func TestHandleSigterm(t *testing.T) {
	cfg, err := os.CreateTemp("", "ptp4u")
	require.NoError(t, err)
	os.Remove(cfg.Name())
	require.NoFileExists(t, cfg.Name())

	c := &Config{StaticConfig: StaticConfig{PidFile: cfg.Name()}}
	s := Server{
		Config: c,
		Stats:  stats.NewJSONStats(1),
	}

	err = c.CreatePidFile()
	require.NoError(t, err)
	require.FileExists(t, c.PidFile)

	// Delayed SIGTERM
	go func() {
		time.Sleep(100 * time.Millisecond)
		err = unix.Kill(unix.Getpid(), unix.SIGTERM)
	}()

	// Must exit the method
	s.handleSigterm()
	require.NoFileExists(t, cfg.Name())
}
