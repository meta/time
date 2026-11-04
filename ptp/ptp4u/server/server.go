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
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/cespare/xxhash/v2"
	ptp "github.com/facebook/time/ptp/protocol"
	"github.com/facebook/time/ptp/ptp4u/drain"
	"github.com/facebook/time/ptp/ptp4u/stats"
	"github.com/facebook/time/timestamp"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Server is PTP unicast server
type Server struct {
	Config *Config
	Stats  stats.Stats
	Checks []drain.Drain
	sw     []*sendWorker

	// drain logic
	cancel context.CancelFunc
	ctx    context.Context
}

// fixed subscription duration for sptp clients
const subscriptionDuration = time.Minute * 5

// Start the workers send bind to event and general UDP ports
func (s *Server) Start() error {
	if err := s.Config.CreatePidFile(); err != nil {
		return err
	}

	// Set clock identity
	iface, err := net.InterfaceByName(s.Config.Interface)
	if err != nil {
		return fmt.Errorf("unable to get mac address of the interface: %w", err)
	}
	s.Config.clockIdentity, err = ptp.NewClockIdentity(iface.HardwareAddr)
	if err != nil {
		return fmt.Errorf("unable to get the Clock Identity (EUI-64 address) of the interface: %w", err)
	}

	// initialize the context for the subscriptions
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Done channel signals the graceful shutdown
	done := make(chan bool)
	// Fail channel signals the failure and shutdown
	fail := make(chan bool)

	// start X workers
	s.sw = make([]*sendWorker, s.Config.SendWorkers)
	for i := 0; i < s.Config.SendWorkers; i++ {
		// Each worker to monitor own queue
		s.sw[i] = newSendWorker(i, s.Config, s.Stats)
		go func(i int) {
			s.sw[i].Start()
			fail <- true
		}(i)
	}

	go func() {
		s.startGeneralListener()
		fail <- true
	}()
	go func() {
		s.startEventListener()
		fail <- true
	}()

	// Drain check
	go func() {
		for ; true; <-time.After(s.Config.DrainInterval) {
			var shouldDrain bool
			for _, check := range s.Checks {
				if check.Check() {
					shouldDrain = true
					log.Warningf("%T engaged", check)
					break
				}
			}

			if drain.Undrain(s.Config.UndrainFileName) {
				log.Warningf("Force undrain file %s is planted, undraining!", s.Config.UndrainFileName)
				shouldDrain = false
			}

			if shouldDrain {
				log.Warning("shifting traffic")
				s.Drain()
				s.Stats.SetDrain(1)
			} else {
				s.Undrain()
				s.Stats.SetDrain(0)
			}
		}
		fail <- true
	}()

	// Watch for SIGHUP and reload dynamic config
	go func() {
		s.handleSighup()
		fail <- true
	}()

	// Watch for SIGTERM and remove pid file
	go func() {
		s.handleSigterm()
		done <- true
	}()

	// Run active metric reporting
	go func() {
		for ; true; <-time.After(s.Config.MetricInterval) {
			for _, w := range s.sw {
				w.inventoryClients()
			}
			s.Stats.SetUTCOffsetSec(int64(s.Config.UTCOffset.Seconds()))
			s.Stats.SetClockAccuracy(int64(s.Config.ClockAccuracy))
			s.Stats.SetClockClass(int64(s.Config.ClockClass))

			s.Stats.Snapshot()
		}
		fail <- true
	}()

	// Wait for ANY goroutine to finish
	select {
	case <-done:
		return nil
	case <-fail:
		return fmt.Errorf("one of server routines finished")
	}
}

// startEventListener launches the listener which listens to subscription requests
func (s *Server) startEventListener() {
	var err error

	iface, err := net.InterfaceByName(s.Config.Interface)
	if err != nil {
		log.Fatalf("failed to get interface: %v", err)
	}

	// socket domain differs depending whether we are listening on ipv4 or ipv6
	domain := unix.AF_INET6
	if s.Config.IP.To4() != nil {
		domain = unix.AF_INET
	}

	fail := make(chan bool)
	for i := 0; i < s.Config.RecvWorkers; i++ {
		eFD, err := unix.Socket(domain, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
		if err != nil {
			log.Fatalf("creating event socket error: %v", err)
		}
		defer unix.Close(eFD)
		sockAddrPort := timestamp.IPToSockaddr(s.Config.IP, ptp.PortEvent)

		// set SO_REUSEPORT to have socket per receive worker
		if err = unix.SetsockoptInt(eFD, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			log.Fatalf("failed to set SO_REUSEPORT on event socket: %v", err)
		}
		// bind to PTP Event port
		if err = unix.Bind(eFD, sockAddrPort); err != nil {
			log.Fatalf("unable to bind event socket connection: %v", err)
		}
		// Enable RX timestamps. Delay requests need to be timestamped by ptp4u on receipt
		if err := timestamp.EnableTimestamps(s.Config.TimestampType, eFD, iface); err != nil {
			log.Fatal(err)
		}

		// Enable SO_RXQ_OVFL to receive socket drops in control messages
		if err := timestamp.EnableRXQueueOverflow(eFD); err != nil {
			log.Warningf("Failed to enable SO_RXQ_OVFL: %v", err)
		}

		err = unix.SetNonblock(eFD, false)
		if err != nil {
			log.Fatalf("Failed to set socket to blocking: %v", err)
		}

		go func(fd, rworker int) {
			s.handleEventMessages(fd, rworker)
			fail <- true
		}(eFD, i)
	}
	<-fail
}

// startGeneralListener launches the listener which listens to announces
func (s *Server) startGeneralListener() {
	// socket domain differs depending whether we are listening on ipv4 or ipv6
	domain := unix.AF_INET6
	if s.Config.IP.To4() != nil {
		domain = unix.AF_INET
	}

	fail := make(chan bool)
	for i := 0; i < s.Config.RecvWorkers; i++ {
		gFD, err := unix.Socket(domain, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
		if err != nil {
			log.Fatalf("creating general socket error: %v", err)
		}
		defer unix.Close(gFD)

		sockAddrPort := timestamp.IPToSockaddr(s.Config.IP, ptp.PortGeneral)

		// set SO_REUSEPORT to have socket per receive worker
		if err = unix.SetsockoptInt(gFD, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			log.Fatalf("failed to set SO_REUSEPORT on general socket: %v", err)
		}
		// bind to PTP Event port
		if err = unix.Bind(gFD, sockAddrPort); err != nil {
			log.Fatalf("unable to bind general socket connection: %v", err)
		}

		err = unix.SetNonblock(gFD, false)
		if err != nil {
			log.Fatalf("Failed to set socket to blocking: %s", err)
		}
		go func(fd int) {
			s.handleGeneralMessages(fd)
			fail <- true
		}(gFD)
	}
	<-fail
}

func readPacketBuf(connFd int, buf []byte) (int, unix.Sockaddr, error) {
	n, saddr, err := unix.Recvfrom(connFd, buf, 0)
	if err != nil {
		return 0, nil, err
	}

	return n, saddr, nil
}

// handleEventMessages is a handler which processes Event Messages in the loop
func (s *Server) handleEventMessages(eFD, rworker int) {
	buf := make([]byte, timestamp.PayloadSizeBytes)
	oob := make([]byte, timestamp.ControlSizeBytes)
	dReq := &ptp.SyncDelayReq{}
	zerotlv := []ptp.TLV{}
	var msgType ptp.MessageType
	var worker *sendWorker
	var sc *SubscriptionClient
	var gclisa unix.Sockaddr
	var expire time.Time
	var workerOffset uint64

	localSockAddr, err := unix.Getsockname(eFD)
	if err != nil {
		log.Errorf("unable to find local ip: %v", err)
	}
	for {
		bbuf, boob, eclisa, err := timestamp.ReadPacketWithCMsgBuf(eFD, buf, oob)
		if err != nil {
			log.Errorf("Failed to read packet on %s: %v", localSockAddr, err)
			continue
		}

		// Get the timestamp
		rxTS, err := timestamp.ReadRXTimestamp(oob, boob)
		if err != nil {
			log.Errorf("Failed to read packet on %s: %v", localSockAddr, err)
			continue
		}
		drops := timestamp.ReadSocketDrops(oob, boob)
		s.Stats.SetRXDrops(rworker, int64(drops))

		if s.Config.TimestampType != timestamp.HW {
			rxTS = rxTS.Add(s.Config.UTCOffset)
		}

		msgType, err = ptp.ProbeMsgType(buf[:bbuf])
		if err != nil {
			log.Errorf("Failed to probe the ptp message type: %v", err)
			continue
		}

		s.Stats.IncRX(msgType)

		// Don't respond on event (delay) requests while being drained
		if s.ctx.Err() != nil {
			continue
		}

		switch msgType {
		case ptp.MessageDelayReq:
			dReq.TLVs = zerotlv
			workerOffset = 0
			if err := ptp.FromBytes(buf[:bbuf], dReq); err != nil {
				log.Errorf("Failed to read the ptp SyncDelayReq: %v", err)
				continue
			}
			log.Debug("Got delay request")
			// CSPTP AlternateResponsePortTLV POC
			for _, tlv := range dReq.TLVs {
				switch v := tlv.(type) {
				case *ptp.AlternateResponsePortTLV:
					workerOffset = uint64(v.Offset)
				}
			}

			worker = s.findWorker(dReq.SourcePortIdentity, workerOffset)
			if dReq.FlagField == ptp.FlagProfileSpecific1|ptp.FlagUnicast {
				expire = time.Now().Add(subscriptionDuration)
				// SYNC DELAY_REQUEST and ANNOUNCE
				// stored by identity alone, so a moved client is still found; the offset only picks the sender
				home := s.findWorker(dReq.SourcePortIdentity, 0)
				sc = home.FindSubscription(dReq.SourcePortIdentity, ptp.MessageDelayReq)
				if sc == nil || sc.queue != worker.queue {
					// the client moved to another sending worker
					if sc != nil {
						sc.Stop()
					}
					// if the port number is > 10, it's a ptping request which expects announce to come to the same ephemeral port
					if dReq.SourcePortIdentity.PortNumber > 10 {
						gclisa = eclisa
					} else {
						gclisa = timestamp.NewSockaddrWithPort(eclisa, ptp.PortGeneral)
					}
					// Create a new subscription
					sc = NewSubscriptionClient(worker.queue, worker.signalingQueue, eclisa, gclisa, ptp.MessageDelayReq, s.Config, subscriptionDuration, expire)
					home.RegisterSubscription(dReq.SourcePortIdentity, ptp.MessageDelayReq, sc)
					go sc.Start(s.ctx)
				} else {
					// bump the subscription; sptp is stateless, port can change
					sc.Bump(expire, eclisa)
					// if the port number is > 10, it's a ptping request which expects announce to come to the same ephemeral port
					if dReq.SourcePortIdentity.PortNumber > 10 {
						sc.SetGclisa(eclisa)
					}
				}
				sc.UpdateSyncDelayReq(rxTS, dReq.SequenceID)
				sc.UpdateAnnounceDelayReq(dReq.CorrectionField, dReq.SequenceID)
			} else {
				// DELAY_RESPONSE
				if sc = worker.FindSubscription(dReq.SourcePortIdentity, ptp.MessageDelayResp); sc == nil {
					log.Infof("Delay request from %s is not in the subscription list", timestamp.SockaddrToIP(eclisa))
					continue
				}
				sc.UpdateDelayResp(&dReq.Header, rxTS)
			}
			sc.Once()
		default:
			log.Warningf("Got unsupported message type %s(%d)", msgType, msgType)
		}
	}
}

// handleGeneralMessage is a handler which processes General Messages arriving in the loop
func (s *Server) handleGeneralMessages(gFd int) {
	buf := make([]byte, timestamp.PayloadSizeBytes)
	signaling := &ptp.Signaling{}
	zerotlv := []ptp.TLV{}

	var signalingType ptp.MessageType
	var durationt time.Duration
	var intervalt time.Duration
	var expire time.Time
	var worker *sendWorker
	var sc *SubscriptionClient

	localSockAddr, err := unix.Getsockname(gFd)
	if err != nil {
		log.Errorf("unable to find local ip: %v", err)
	}

	for {
		bbuf, gclisa, err := readPacketBuf(gFd, buf)
		if err != nil {
			log.Errorf("Failed to read packet on %s: %v", localSockAddr, err)
			continue
		}

		msgType, err := ptp.ProbeMsgType(buf[:bbuf])
		if err != nil {
			log.Errorf("Failed to probe the ptp message type: %v", err)
			continue
		}

		switch msgType {
		case ptp.MessageSignaling:
			signaling.TLVs = zerotlv
			if err := ptp.FromBytes(buf[:bbuf], signaling); err != nil {
				log.Error(err)
				continue
			}

			for _, tlv := range signaling.TLVs {
				switch v := tlv.(type) {
				case *ptp.RequestUnicastTransmissionTLV:
					signalingType = v.MsgTypeAndReserved.MsgType()
					s.Stats.IncRXSignalingGrant(signalingType)
					log.Debugf("Got %s grant request", signalingType)
					durationt = time.Duration(v.DurationField) * time.Second
					expire = time.Now().Add(durationt)
					intervalt = v.LogInterMessagePeriod.Duration()

					switch signalingType {
					case ptp.MessageAnnounce, ptp.MessageSync, ptp.MessageDelayResp:
						// Decide before touching the subscription: a denied renewal must not
						// change the interval, expiry or address of a running one
						withinLimits := intervalt >= s.Config.MinSubInterval && durationt <= s.Config.MaxSubDuration && s.ctx.Err() == nil
						worker = s.findWorker(signaling.SourcePortIdentity, 0)
						sc = worker.FindSubscription(signaling.SourcePortIdentity, signalingType)
						if sc == nil || !sc.Running() {
							ip := timestamp.SockaddrToIP(gclisa)
							eclisa := timestamp.IPToSockaddr(ip, ptp.PortEvent)
							sc = NewSubscriptionClient(worker.queue, worker.signalingQueue, eclisa, gclisa, signalingType, s.Config, intervalt, expire)
							worker.RegisterSubscription(signaling.SourcePortIdentity, signalingType, sc)
						} else if withinLimits {
							// Update existing subscription data
							sc.SetExpire(expire)
							sc.SetInterval(intervalt)
							// Update gclisa in case of renewal. This is against the standard,
							// but we want to be able to respond to DelayResps coming from ephemeral ports
							sc.SetGclisa(gclisa)
						}

						// Reject queries out of limit
						if !withinLimits {
							sc.sendSignalingGrant(signaling, v.MsgTypeAndReserved, v.LogInterMessagePeriod, 0)
							continue
						}

						// Send confirmation grant
						sc.sendSignalingGrant(signaling, v.MsgTypeAndReserved, v.LogInterMessagePeriod, v.DurationField)

						if !sc.Running() {
							go sc.Start(s.ctx)
						}
					default:
						log.Errorf("Got unsupported grant type %s", signalingType)
					}
				case *ptp.CancelUnicastTransmissionTLV:
					signalingType = v.MsgTypeAndFlags.MsgType()
					s.Stats.IncRXSignalingCancel(signalingType)
					log.Debugf("Got %s cancel request", signalingType)
					worker = s.findWorker(signaling.SourcePortIdentity, 0)
					sc = worker.FindSubscription(signaling.SourcePortIdentity, signalingType)
					if sc != nil {
						sc.Stop()
					}
				case *ptp.AcknowledgeCancelUnicastTransmissionTLV:
					log.Debugf("Got %s acknowledge cancel request", signalingType)
				default:
					log.Errorf("Got unsupported message type %s(%d)", msgType, msgType)
				}
			}
		}
	}
}

func (s *Server) findWorker(clientID ptp.PortIdentity, offset uint64) *sendWorker {
	val := uint64(clientID.ClockIdentity) + uint64(clientID.PortNumber) + offset
	hashableBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(hashableBytes, val)
	hash := xxhash.Sum64(hashableBytes)
	return s.sw[hash%uint64(s.Config.SendWorkers)] //#nosec G115
}

// Drain traffic
func (s *Server) Drain() {
	if s.ctx != nil && s.ctx.Err() == nil {
		s.cancel()
	}

	for range 10 {
		// Verifying all subscriptions are over
		for _, w := range s.sw {
			if n := w.inventoryClients(); n != 0 {
				log.Warningf("Still waiting for %d subscriptions on worker %d to finish...", n, w.id)
				time.Sleep(time.Second)
			}
		}
	}
}

// Undrain traffic
func (s *Server) Undrain() {
	if s.ctx != nil && s.ctx.Err() != nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
}

// handleSighup watches for SIGHUP and reloads the dynamic config
func (s *Server) handleSighup() {
	log.Info("Engaging the SIGHUP monitoring")
	sigchan := make(chan os.Signal, 10)
	signal.Notify(sigchan, unix.SIGHUP)
	for range sigchan {
		log.Info("SIGHUP received, reloading config")
		dc, err := ReadDynamicConfig(s.Config.ConfigFile)
		if err != nil {
			log.Errorf("Failed to reload config: %v. Moving on", err)
			continue
		}
		dcMux.Lock()
		s.Config.DynamicConfig = *dc
		dcMux.Unlock()

		s.Stats.IncReload()
	}
}

// handleSigterm watches for SIGTERM and SIGINT and removes the pid file
func (s *Server) handleSigterm() {
	log.Info("Engaging the SIGTERM/SIGINT monitoring")
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, unix.SIGTERM, unix.SIGINT)
	<-sigchan
	log.Warning("Shutting down ptp4u")

	log.Info("Initiating drain")
	s.Drain()

	log.Info("Removing pid")
	if err := s.Config.DeletePidFile(); err != nil {
		log.Fatalf("Failed to remove pid file: %v", err)
	}
}
