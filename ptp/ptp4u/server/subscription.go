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
Package server implements simple Unicast PTP UDP server.
*/
package server

import (
	"context"
	"encoding/binary"
	"net/netip"
	"sync"
	"time"

	ptp "github.com/facebook/time/ptp/protocol"
	log "github.com/sirupsen/logrus"
)

// SubscriptionClient is sending subscriptionType messages periodically
type SubscriptionClient struct {
	sync.Mutex

	queue            chan *SubscriptionClient
	signalingQueue   chan *SubscriptionClient
	subscriptionType ptp.MessageType
	serverConfig     *Config

	interval   time.Duration
	expire     time.Time
	sequenceID uint16
	running    bool
	stop       chan bool

	runningInterval time.Duration
	intervalTicker  *time.Ticker

	// client addresses, guarded by the mutex: receivers move them while a worker sends
	eaddr netip.AddrPort
	gaddr netip.AddrPort

	// packets
	syncP      *ptp.SyncDelayReq
	followupP  *ptp.FollowUp
	announceP  *ptp.Announce
	delayRespP *ptp.DelayResp
	signaling  *ptp.Signaling
}

// NewSubscriptionClient gets minimal required arguments to create a subscription
func NewSubscriptionClient(q chan *SubscriptionClient, gq chan *SubscriptionClient, eaddr, gaddr netip.AddrPort, st ptp.MessageType, sc *Config, i time.Duration, e time.Time) *SubscriptionClient {
	s := &SubscriptionClient{
		eaddr:            eaddr,
		gaddr:            gaddr,
		subscriptionType: st,
		interval:         i,
		expire:           e,
		queue:            q,
		signalingQueue:   gq,
		serverConfig:     sc,
		stop:             make(chan bool, 1),
	}
	s.initSync()
	s.initFollowup()
	s.initAnnounce()
	s.initDelayResp()
	s.initSignaling()

	return s
}

// Start launches the subscription timers and exit on expire
func (sc *SubscriptionClient) Start(ctx context.Context) {
	eaddr, _ := sc.Addrs()
	ip := eaddr.Addr().Unmap()
	log.Infof("Starting a new %s subscription for %s", sc.subscriptionType, ip)
	sc.setRunning(true)

	// Send first message right away
	if sc.subscriptionType != ptp.MessageDelayResp && sc.subscriptionType != ptp.MessageDelayReq {
		sc.Once()
	}

	sc.runningInterval = sc.interval
	sc.intervalTicker = time.NewTicker(sc.runningInterval)

	defer log.Infof("Subscription %s is over for %s", sc.subscriptionType, ip)
	if sc.subscriptionType != ptp.MessageDelayReq {
		defer sc.sendSignalingCancel()
	}
	defer sc.intervalTicker.Stop()
	defer sc.setRunning(false)

	for {
		select {
		case <-ctx.Done():
			return
		case <-sc.stop:
			return
		case <-sc.intervalTicker.C:
			if sc.Expired() {
				return
			}

			// check if interval changed, maybe update our ticker
			if sc.runningInterval != sc.interval {
				sc.runningInterval = sc.interval
				sc.intervalTicker.Reset(sc.runningInterval)
			}
			if sc.subscriptionType != ptp.MessageDelayResp && sc.subscriptionType != ptp.MessageDelayReq {
				// Add myself to the worker queue
				sc.Once()
			}
		}
	}
}

// Once adds itself to the worker queue once
func (sc *SubscriptionClient) Once() {
	sc.queue <- sc
}

// OnceSignaling adds itself to the worker signaling queue once
func (sc *SubscriptionClient) OnceSignaling() {
	sc.signalingQueue <- sc
}

// Expired checks if the subscription expired or not
func (sc *SubscriptionClient) Expired() bool {
	sc.Lock()
	defer sc.Unlock()
	return time.Now().After(sc.expire)
}

// Stop stops the subscription
func (sc *SubscriptionClient) Stop() {
	sc.Lock()
	defer sc.Unlock()
	// Make sure we mark subscription as expired
	sc.expire = time.Now()
	// And demand subscription stop
	if sc.running {
		sc.stop <- true
	}
}

// setRunning atomically sets running
func (sc *SubscriptionClient) setRunning(running bool) {
	sc.Lock()
	defer sc.Unlock()
	sc.running = running
}

// SetExpire atomically sets expire
func (sc *SubscriptionClient) SetExpire(expire time.Time) {
	sc.Lock()
	defer sc.Unlock()
	sc.expire = expire
}

// SetInterval atomically sets interval
func (sc *SubscriptionClient) SetInterval(interval time.Duration) {
	sc.Lock()
	defer sc.Unlock()
	sc.interval = interval
}

// Bump atomically sets expire and eaddr
func (sc *SubscriptionClient) Bump(expire time.Time, eaddr netip.AddrPort) {
	sc.Lock()
	defer sc.Unlock()
	sc.expire = expire
	sc.eaddr = eaddr
}

// Addrs atomically returns the event and general addresses of the client
func (sc *SubscriptionClient) Addrs() (netip.AddrPort, netip.AddrPort) {
	sc.Lock()
	defer sc.Unlock()
	return sc.eaddr, sc.gaddr
}

// SetGaddr atomically sets gaddr
func (sc *SubscriptionClient) SetGaddr(gaddr netip.AddrPort) {
	sc.Lock()
	defer sc.Unlock()
	sc.gaddr = gaddr
}

// Running returns the running bool
func (sc *SubscriptionClient) Running() bool {
	sc.Lock()
	defer sc.Unlock()
	return sc.running
}

// IncSequenceID adds 1 to a sequence id
func (sc *SubscriptionClient) IncSequenceID() {
	sc.sequenceID++
}

func (sc *SubscriptionClient) initSync() {
	sc.syncP = &ptp.SyncDelayReq{
		Header: ptp.Header{
			SdoIDAndMsgType: ptp.NewSdoIDAndMsgType(ptp.MessageSync, 0),
			Version:         ptp.Version,
			MessageLength:   uint16(binary.Size(ptp.Header{}) + binary.Size(ptp.SyncDelayReqBody{})), //#nosec G115
			DomainNumber:    uint8(sc.serverConfig.DomainNumber),
			FlagField:       ptp.FlagUnicast | ptp.FlagTwoStep,
			SequenceID:      0,
			SourcePortIdentity: ptp.PortIdentity{
				PortNumber:    1,
				ClockIdentity: sc.serverConfig.clockIdentity,
			},
			LogMessageInterval: 0x7f,
			ControlField:       0,
		},
	}
}

// UpdateSync updates ptp Sync packet
func (sc *SubscriptionClient) UpdateSync() {
	sc.syncP.SequenceID = sc.sequenceID
}

// UpdateSyncDelayReq updates ptp SyncDelayReq packet
func (sc *SubscriptionClient) UpdateSyncDelayReq(received time.Time, seq uint16) {
	sc.syncP.SequenceID = seq
	sc.syncP.OriginTimestamp = ptp.NewTimestamp(received)
}

// Sync returns ptp Sync packet
func (sc *SubscriptionClient) Sync() *ptp.SyncDelayReq {
	return sc.syncP
}

func (sc *SubscriptionClient) initFollowup() {
	sc.followupP = &ptp.FollowUp{
		Header: ptp.Header{
			SdoIDAndMsgType: ptp.NewSdoIDAndMsgType(ptp.MessageFollowUp, 0),
			Version:         ptp.Version,
			MessageLength:   uint16(binary.Size(ptp.FollowUp{})),
			DomainNumber:    uint8(sc.serverConfig.DomainNumber),
			FlagField:       ptp.FlagUnicast,
			SequenceID:      0,
			SourcePortIdentity: ptp.PortIdentity{
				PortNumber:    1,
				ClockIdentity: sc.serverConfig.clockIdentity,
			},
			LogMessageInterval: 0,
			ControlField:       2,
		},
		FollowUpBody: ptp.FollowUpBody{
			PreciseOriginTimestamp: ptp.NewTimestamp(time.Now()),
		},
	}
}

// UpdateFollowup updates ptp Follow Up packet
func (sc *SubscriptionClient) UpdateFollowup(hwts time.Time) {
	i, _ := ptp.NewLogInterval(sc.interval)
	sc.followupP.SequenceID = sc.sequenceID
	sc.followupP.LogMessageInterval = i
	sc.followupP.PreciseOriginTimestamp = ptp.NewTimestamp(hwts)
}

// Followup returns ptp Follow Up packet
func (sc *SubscriptionClient) Followup() *ptp.FollowUp {
	return sc.followupP
}

func (sc *SubscriptionClient) initAnnounce() {
	sc.announceP = &ptp.Announce{
		Header: ptp.Header{
			SdoIDAndMsgType: ptp.NewSdoIDAndMsgType(ptp.MessageAnnounce, 0),
			Version:         ptp.Version,
			MessageLength:   uint16(binary.Size(ptp.Header{}) + binary.Size(ptp.AnnounceBody{})),
			DomainNumber:    uint8(sc.serverConfig.DomainNumber),
			FlagField:       ptp.FlagUnicast | ptp.FlagPTPTimescale,
			SequenceID:      0,
			SourcePortIdentity: ptp.PortIdentity{
				PortNumber:    1,
				ClockIdentity: sc.serverConfig.clockIdentity,
			},
			LogMessageInterval: 0,
			ControlField:       5,
		},
		AnnounceBody: ptp.AnnounceBody{
			CurrentUTCOffset:     0,
			Reserved:             0,
			GrandmasterPriority1: 128,
			GrandmasterClockQuality: ptp.ClockQuality{
				ClockClass:              0,
				ClockAccuracy:           0,
				OffsetScaledLogVariance: 23008,
			},
			GrandmasterPriority2: 128,
			GrandmasterIdentity:  sc.serverConfig.clockIdentity,
			StepsRemoved:         0,
			TimeSource:           ptp.TimeSourceGNSS,
		},
	}
}

// UpdateAnnounce updates ptp Announce packet
func (sc *SubscriptionClient) UpdateAnnounce() {
	i, _ := ptp.NewLogInterval(sc.interval)
	sc.announceP.SequenceID = sc.sequenceID
	sc.announceP.LogMessageInterval = i
	sc.announceP.CurrentUTCOffset = int16(sc.serverConfig.UTCOffset.Seconds())
	sc.announceP.GrandmasterClockQuality.ClockClass = sc.serverConfig.ClockClass
	sc.announceP.GrandmasterClockQuality.ClockAccuracy = sc.serverConfig.ClockAccuracy
}

// UpdateAnnounceDelayReq updates ptp Announce Delay Req payload
func (sc *SubscriptionClient) UpdateAnnounceDelayReq(cf ptp.Correction, seq uint16) {
	sc.announceP.SequenceID = seq
	sc.announceP.CurrentUTCOffset = int16(sc.serverConfig.UTCOffset.Seconds())
	sc.announceP.GrandmasterClockQuality.ClockClass = sc.serverConfig.ClockClass
	sc.announceP.GrandmasterClockQuality.ClockAccuracy = sc.serverConfig.ClockAccuracy
	sc.announceP.CorrectionField = cf
}

// UpdateAnnounceFollowUp updates ptp Announce Follow Up payload
func (sc *SubscriptionClient) UpdateAnnounceFollowUp(transmitted time.Time) {
	sc.announceP.OriginTimestamp = ptp.NewTimestamp(transmitted)
}

// Announce returns ptp Announce packet
func (sc *SubscriptionClient) Announce() *ptp.Announce {
	return sc.announceP
}

func (sc *SubscriptionClient) initDelayResp() {
	sc.delayRespP = &ptp.DelayResp{
		Header: ptp.Header{
			SdoIDAndMsgType: ptp.NewSdoIDAndMsgType(ptp.MessageDelayResp, 0),
			Version:         ptp.Version,
			MessageLength:   uint16(binary.Size(ptp.DelayResp{})),
			DomainNumber:    uint8(sc.serverConfig.DomainNumber),
			FlagField:       ptp.FlagUnicast,
			SequenceID:      0,
			SourcePortIdentity: ptp.PortIdentity{
				PortNumber:    1,
				ClockIdentity: sc.serverConfig.clockIdentity,
			},
			LogMessageInterval: 0x7f,
			ControlField:       3,
			CorrectionField:    0,
		},
		DelayRespBody: ptp.DelayRespBody{},
	}
}

// UpdateDelayResp updates ptp Delay Response packet
func (sc *SubscriptionClient) UpdateDelayResp(h *ptp.Header, received time.Time) {
	sc.delayRespP.SequenceID = h.SequenceID
	sc.delayRespP.CorrectionField = h.CorrectionField
	sc.delayRespP.DelayRespBody = ptp.DelayRespBody{
		ReceiveTimestamp:       ptp.NewTimestamp(received),
		RequestingPortIdentity: h.SourcePortIdentity,
	}
}

// DelayResp returns ptp Delay Response packet
func (sc *SubscriptionClient) DelayResp() *ptp.DelayResp {
	return sc.delayRespP
}

func (sc *SubscriptionClient) initSignaling() {
	sc.signaling = &ptp.Signaling{
		Header: ptp.Header{
			Version:       ptp.Version,
			MessageLength: uint16(binary.Size(ptp.Header{}) + binary.Size(ptp.PortIdentity{}) + binary.Size(ptp.GrantUnicastTransmissionTLV{})),
			FlagField:     ptp.FlagUnicast,
			SourcePortIdentity: ptp.PortIdentity{
				PortNumber:    1,
				ClockIdentity: sc.serverConfig.clockIdentity,
			},
		},
		TargetPortIdentity: ptp.PortIdentity{},
		TLVs:               []ptp.TLV{},
	}
}

// UpdateSignalingGrant updates ptp Signaling packet granting the requested subscription
func (sc *SubscriptionClient) UpdateSignalingGrant(sg *ptp.Signaling, mt ptp.UnicastMsgTypeAndFlags, interval ptp.LogInterval, duration uint32) {
	sc.signaling.MessageLength = uint16(binary.Size(ptp.Header{}) + binary.Size(ptp.PortIdentity{}) + binary.Size(ptp.GrantUnicastTransmissionTLV{}))
	sc.signaling.SdoIDAndMsgType = sg.SdoIDAndMsgType
	sc.signaling.DomainNumber = sg.DomainNumber
	sc.signaling.MinorSdoID = sg.MinorSdoID
	sc.signaling.CorrectionField = sg.CorrectionField
	sc.signaling.MessageTypeSpecific = sg.MessageTypeSpecific
	sc.signaling.SequenceID = sg.SequenceID
	sc.signaling.ControlField = sg.ControlField
	sc.signaling.LogMessageInterval = sg.LogMessageInterval

	sc.signaling.TargetPortIdentity = sg.SourcePortIdentity
	sc.signaling.TLVs = []ptp.TLV{
		&ptp.GrantUnicastTransmissionTLV{
			TLVHead:               ptp.TLVHead{TLVType: ptp.TLVGrantUnicastTransmission, LengthField: uint16(binary.Size(ptp.GrantUnicastTransmissionTLV{}) - binary.Size(ptp.TLVHead{}))},
			Reserved:              0,
			Renewal:               1,
			MsgTypeAndReserved:    mt,
			LogInterMessagePeriod: interval,
			DurationField:         duration,
		},
	}
}

// UpdateSignalingCancel updates ptp Signaling packet canceling the requested subscription
func (sc *SubscriptionClient) UpdateSignalingCancel() {
	sc.signaling.MessageLength = uint16(binary.Size(ptp.Header{}) + binary.Size(ptp.PortIdentity{}) + binary.Size(ptp.CancelUnicastTransmissionTLV{}))
	sc.signaling.TLVs = []ptp.TLV{
		&ptp.CancelUnicastTransmissionTLV{
			TLVHead:         ptp.TLVHead{TLVType: ptp.TLVCancelUnicastTransmission, LengthField: uint16(binary.Size(ptp.CancelUnicastTransmissionTLV{}) - binary.Size(ptp.TLVHead{}))},
			Reserved:        0,
			MsgTypeAndFlags: ptp.NewUnicastMsgTypeAndFlags(sc.subscriptionType, 0),
		},
	}
}

// Signaling returns ptp Signaling packet granting the requested subscription
func (sc *SubscriptionClient) Signaling() *ptp.Signaling {
	return sc.signaling
}

// sendSignalingGrant sends a Unicast Grant message
func (sc *SubscriptionClient) sendSignalingGrant(sg *ptp.Signaling, mt ptp.UnicastMsgTypeAndFlags, interval ptp.LogInterval, duration uint32) {
	sc.UpdateSignalingGrant(sg, mt, interval, duration)
	sc.OnceSignaling()
}

// sendSignalingCancel sends a Unicast Cancel message
func (sc *SubscriptionClient) sendSignalingCancel() {
	sc.UpdateSignalingCancel()
	sc.OnceSignaling()
}
