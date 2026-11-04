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
	"math"
	"net"
	"os"
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
	moved, err := unix.Getsockname(movedFD)
	require.NoError(t, err)
	gclisa := func() unix.Sockaddr {
		sc.Lock()
		defer sc.Unlock()
		return sc.gclisa
	}
	require.Eventually(t, func() bool {
		if timestamp.SockaddrToPort(gclisa()) == timestamp.SockaddrToPort(moved) {
			return true
		}
		_ = unix.Sendto(movedFD, req, 0, serverAddr)
		return false
	}, time.Second, 100*time.Millisecond, "a ptping refresh must send announce to the new ephemeral port")
	require.Equal(t, timestamp.SockaddrToPort(moved), timestamp.SockaddrToPort(sc.Eclisa()))
	require.True(t, home.FindSubscription(client, ptp.MessageDelayReq) == sc, "a refresh must keep the subscription")
}

func eventServer(t *testing.T) (*Server, unix.Sockaddr) {
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

func loopbackSocket(t *testing.T) int {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, timestamp.IPToSockaddr(net.ParseIP("127.0.0.1"), 0)))
	return fd
}

func delayReq(t *testing.T, client ptp.PortIdentity, offset uint16) []byte {
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

func grantRequest(t *testing.T, client ptp.PortIdentity, interval ptp.LogInterval, duration uint32) []byte {
	size := binary.Size(ptp.Header{}) + binary.Size(ptp.PortIdentity{}) + binary.Size(ptp.RequestUnicastTransmissionTLV{})
	req := &ptp.Signaling{
		Header: ptp.Header{
			SdoIDAndMsgType:    ptp.NewSdoIDAndMsgType(ptp.MessageSignaling, 0),
			Version:            ptp.Version,
			MessageLength:      uint16(size), //#nosec G115
			FlagField:          ptp.FlagUnicast,
			SourcePortIdentity: client,
			LogMessageInterval: 0x7f,
		},
		TargetPortIdentity: ptp.PortIdentity{PortNumber: 0xffff, ClockIdentity: 0xffffffffffffffff},
		TLVs: []ptp.TLV{&ptp.RequestUnicastTransmissionTLV{
			TLVHead: ptp.TLVHead{
				TLVType:     ptp.TLVRequestUnicastTransmission,
				LengthField: uint16(binary.Size(ptp.RequestUnicastTransmissionTLV{}) - binary.Size(ptp.TLVHead{})), //#nosec G115
			},
			MsgTypeAndReserved:    ptp.NewUnicastMsgTypeAndFlags(ptp.MessageAnnounce, 0),
			LogInterMessagePeriod: interval,
			DurationField:         duration,
		}},
	}
	buf := make([]byte, size+ptp.TrailingBytes)
	n, err := ptp.BytesTo(req, buf)
	require.NoError(t, err)
	return buf[:n]
}

func TestSignalingDeniedRenewalKeepsSubscription(t *testing.T) {
	c := &Config{
		clockIdentity: ptp.ClockIdentity(1234),
		StaticConfig:  StaticConfig{TimestampType: timestamp.SW, SendWorkers: 10, QueueSize: 10},
		DynamicConfig: DynamicConfig{MinSubInterval: time.Second, MaxSubDuration: time.Hour},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	s := &Server{Config: c, Stats: stats.NewJSONStats(1), sw: make([]*sendWorker, c.SendWorkers), ctx: ctx, cancel: cancel}
	for i := range s.sw {
		s.sw[i] = newSendWorker(i, c, s.Stats)
	}

	// left open: closing it would spin handleGeneralMessages on read errors
	gFD := loopbackSocket(t)
	serverAddr, err := unix.Getsockname(gFD)
	require.NoError(t, err)
	go s.handleGeneralMessages(gFD)

	clientFD := loopbackSocket(t)
	defer unix.Close(clientFD)
	clientAddr, err := unix.Getsockname(clientFD)
	require.NoError(t, err)
	client := ptp.PortIdentity{ClockIdentity: 5678, PortNumber: 1}
	worker := s.findWorker(client, 0)

	nextGrant := func() *ptp.GrantUnicastTransmissionTLV {
		select {
		case g := <-worker.signalingQueue:
			return g.Signaling().TLVs[0].(*ptp.GrantUnicastTransmissionTLV)
		case <-time.After(time.Second):
			t.Fatal("no grant sent")
			return nil
		}
	}

	require.NoError(t, unix.Sendto(clientFD, grantRequest(t, client, 0, 60), 0, serverAddr))
	require.Equal(t, uint32(60), nextGrant().DurationField)
	var sc *SubscriptionClient
	require.Eventually(t, func() bool {
		sc = worker.FindSubscription(client, ptp.MessageAnnounce)
		return sc != nil && sc.Running()
	}, time.Second, 10*time.Millisecond)

	// renewal from another port, faster than MinSubInterval and longer than MaxSubDuration
	otherFD := loopbackSocket(t)
	defer unix.Close(otherFD)
	require.NoError(t, unix.Sendto(otherFD, grantRequest(t, client, -7, math.MaxUint32), 0, serverAddr))
	require.Equal(t, uint32(0), nextGrant().DurationField, "the renewal must be denied")

	sc.Lock()
	interval, expire, gclisa := sc.interval, sc.expire, sc.gclisa
	sc.Unlock()
	require.Equal(t, time.Second, interval, "a denied renewal must not change the interval")
	require.LessOrEqual(t, time.Until(expire), time.Minute, "a denied renewal must not extend the subscription")
	require.Equal(t, timestamp.SockaddrToPort(clientAddr), timestamp.SockaddrToPort(gclisa), "a denied renewal must not move the subscription")
	require.True(t, sc.Running())
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
	sa := timestamp.IPToSockaddr(net.ParseIP("127.0.0.1"), 123)
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
