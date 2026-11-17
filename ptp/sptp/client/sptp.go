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

package client

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/facebook/time/phc"
	ptp "github.com/facebook/time/ptp/protocol"
	"github.com/facebook/time/ptp/sptp/asymmetry"
	"github.com/facebook/time/servo"
	"github.com/facebook/time/timestamp"
)

// Servo abstracts away servo
type Servo interface {
	SyncInterval(float64)
	Sample(offset int64, localTs uint64) (float64, servo.State)
	SetMaxFreq(float64)
	SetLastFreq(float64)
	MeanFreq() float64
	IsSpike(offset int64) bool
	GetState() servo.State
	UnsetFirstUpdate()
	IsStable(offset int64) bool
}

// SPTP is a Simple Unicast PTP client
type SPTP struct {
	cfg *Config

	pi Servo

	stats StatsServer

	clock Clock

	bestGM netip.Addr

	clients    map[netip.Addr]*Client
	priorities map[netip.Addr]int
	backoff    map[netip.Addr]*backoff
	lastTick   time.Time
	isStalled  bool

	pinger pingState
	// pdelaySrc is the source multicast peer delay probes must come from, by family
	pdelaySrc map[bool]netip.Addr
	// corrector is nil when asymmetry correction is disabled
	corrector asymmetry.Corrector

	clockID ptp.ClockIdentity
	genConn UDPConnNoTS
	// connections on port 319
	// if parallel TX is enabled, there will be one connection per server, as TX timestamping happens per socket.
	// otherwise, there will be only one connection for all servers.
	eventConns []UDPConnWithTS
}

// NewSPTP creates SPTP client
func NewSPTP(cfg *Config, stats StatsServer) (*SPTP, error) {
	p := &SPTP{
		cfg:   cfg,
		stats: stats,
	}
	p.pinger.init(cfg)
	if err := p.init(); err != nil {
		return nil, err
	}
	if err := p.initClients(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *SPTP) initClients() error {
	iface, err := net.InterfaceByName(p.cfg.Iface)
	if err != nil {
		return err
	}
	p.clients = map[netip.Addr]*Client{}
	p.priorities = map[netip.Addr]int{}
	p.backoff = map[netip.Addr]*backoff{}
	if p.cfg.ParallelTX {
		log.Info("Initialising clients with parallel TX feature")
	}
	for server, prio := range p.cfg.Servers {
		// normalize the address
		ip, err := LookupNetIP(server)
		if err != nil {
			return fmt.Errorf("parsing server address %q: %w", server, err)
		}
		var econn UDPConnWithTS
		if p.cfg.ParallelTX {
			econn, err = NewUDPConnTS(net.ParseIP(p.cfg.ListenAddress), ptp.PortEvent, p.cfg.Timestamping, iface, p.cfg.DSCP)
			if err != nil {
				return err
			}
			if err := ConfigPktInfo(econn.ConnFd()); err != nil {
				log.Warningf("failed to configure pktinfo: %v", err)
			}
			loopLevel, loopOpt := unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_LOOP
			if err := unix.SetsockoptInt(econn.ConnFd(), loopLevel, loopOpt, 0); err != nil {
				log.Warningf("disabling IPv6 multicast loopback: %v", err)
			}
			loopLevel, loopOpt = unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP
			if err := unix.SetsockoptInt(econn.ConnFd(), loopLevel, loopOpt, 0); err != nil {
				log.Warningf("disabling IPv4 multicast loopback: %v", err)
			}
			// keep track of the event connections
			p.eventConns = append(p.eventConns, econn)
		} else {
			econn = p.eventConns[0]
		}
		c, err := NewClient(ip, ptp.PortEvent, p.clockID, econn, p.genConn, p.cfg, p.stats)
		if err != nil {
			return fmt.Errorf("initializing client %v: %w", ip, err)
		}
		p.clients[ip] = c
		p.priorities[ip] = prio
		p.backoff[ip] = newBackoff(p.cfg.Backoff)
	}

	// Join multicast group(s) based on server address families
	hasIPv4 := false
	hasIPv6 := false
	for addr := range p.clients {
		if addr.Is4() || addr.Is4In6() {
			hasIPv4 = true
		} else {
			hasIPv6 = true
		}
	}
	mcastFd := p.eventConns[0].ConnFd()
	if hasIPv4 {
		if err := timestamp.JoinMulticast(mcastFd, iface, net.ParseIP(ptp.PDelayMulticastIPv4)); err != nil {
			log.Warningf("failed to join IPv4 peer delay multicast group %s: %v", ptp.PDelayMulticastIPv4, err)
		} else {
			log.Debugf("joined PTP peer delay multicast group %s on interface %s", ptp.PDelayMulticastIPv4, iface.Name)
		}
	}
	if hasIPv6 {
		if err := timestamp.JoinMulticast(mcastFd, iface, net.ParseIP(ptp.PDelayMulticastIPv6)); err != nil {
			log.Warningf("failed to join IPv6 peer delay multicast group %s: %v", ptp.PDelayMulticastIPv6, err)
		} else {
			log.Debugf("joined PTP peer delay multicast group %s on interface %s", ptp.PDelayMulticastIPv6, iface.Name)
		}
	}
	if !hasIPv4 && !hasIPv6 {
		log.Warningf("no servers configured, not joining any peer delay multicast group")
	}
	// AddrToSockaddr drops the IPv6 zone, so multicast egress relies entirely on this.
	// Only a family we actually use is worth warning about: the other one legitimately
	// fails on a single-family socket.
	err6, err4 := setMulticastIface(mcastFd, iface)
	if hasIPv6 && err6 != nil {
		log.Warningf("IPv6 multicast egress not pinned to %s: %v", iface.Name, err6)
	}
	if hasIPv4 && err4 != nil {
		log.Warningf("IPv4 multicast egress not pinned to %s: %v", iface.Name, err4)
	}
	return nil
}

// setMulticastIface pins outgoing multicast to iface, reporting each family
// separately so the caller can ignore one it does not use
func setMulticastIface(fd int, iface *net.Interface) (err6, err4 error) {
	if iface.Index < 0 || iface.Index > math.MaxInt32 {
		err := fmt.Errorf("interface index %d out of range", iface.Index)
		return err, err
	}
	idx := int32(iface.Index)
	err6 = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_IF, iface.Index)
	// IP_MULTICAST_IF takes an ip_mreqn, an int would be read as a struct in_addr
	err4 = unix.SetsockoptIPMreqn(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, &unix.IPMreqn{Ifindex: idx})
	return err6, err4
}

// sourceAddrTowards asks the routing table which source a packet to dst would use
func sourceAddrTowards(dst netip.Addr) (netip.Addr, error) {
	c, err := net.Dial("udp", net.JoinHostPort(dst.String(), strconv.Itoa(ptp.PortEvent)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolving source address towards %s: %w", dst, err)
	}
	defer c.Close()
	src, ok := netip.AddrFromSlice(c.LocalAddr().(*net.UDPAddr).IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("unexpected local address %v", c.LocalAddr())
	}
	return src.Unmap(), nil
}

// peerDelaySources is the address rack peers must reply to, per family. A probe
// to the IPv4 group cannot be pinned to an IPv6 source, so both are kept.
func (p *SPTP) peerDelaySources() map[bool]netip.Addr {
	srcs := map[bool]netip.Addr{}
	var errs []error
	for _, server := range slices.Sorted(maps.Keys(p.cfg.Servers)) {
		dst, err := LookupNetIP(server)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, seen := srcs[dst.Unmap().Is4()]; seen {
			continue
		}
		src, err := sourceAddrTowards(dst)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// a hostname resolving to IPv4 yields ::ffff:a.b.c.d, whose source is IPv4
		srcs[src.Unmap().Is4()] = src.Unmap()
	}
	if len(errs) > 0 {
		// probes to the family that failed here report only "no source address"
		log.Warningf("no peer delay source for some families: %v", errors.Join(errs...))
	}
	return srcs
}

func (p *SPTP) init() error {
	iface, err := net.InterfaceByName(p.cfg.Iface)
	if err != nil {
		return err
	}

	cid, err := ptp.NewClockIdentity(iface.HardwareAddr)
	if err != nil {
		return err
	}
	p.clockID = cid

	// only multicast pdelay needs this, so a routing gap must not stop the daemon:
	// Ping fails closed per family instead
	p.pdelaySrc = p.peerDelaySources()
	p.corrector = newCorrector(p.cfg.Asymmetry)

	p.genConn, err = NewUDPConn(net.ParseIP(p.cfg.ListenAddress), ptp.PortGeneral)
	if err != nil {
		return fmt.Errorf("binding to %d: %w", ptp.PortGeneral, err)
	}

	if !p.cfg.ParallelTX {
		// bind to event port
		eventConn, err := NewUDPConnTS(net.ParseIP(p.cfg.ListenAddress), ptp.PortEvent, p.cfg.Timestamping, iface, p.cfg.DSCP)
		if err != nil {
			return fmt.Errorf("binding to %d: %w", ptp.PortEvent, err)
		}
		if err := ConfigPktInfo(eventConn.ConnFd()); err != nil {
			log.Warningf("failed to configure pktinfo: %v", err)
		}
		loopLevel, loopOpt := unix.IPPROTO_IPV6, unix.IPV6_MULTICAST_LOOP
		if err := unix.SetsockoptInt(eventConn.ConnFd(), loopLevel, loopOpt, 0); err != nil {
			log.Warningf("disabling IPv6 multicast loopback: %v", err)
		}
		loopLevel, loopOpt = unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP
		if err := unix.SetsockoptInt(eventConn.ConnFd(), loopLevel, loopOpt, 0); err != nil {
			log.Warningf("disabling IPv4 multicast loopback: %v", err)
		}

		p.eventConns = append(p.eventConns, eventConn)
	}

	// Configure TX timestamp attempts and timemouts
	timestamp.AttemptsTXTS = p.cfg.AttemptsTXTS
	timestamp.TimeoutTXTS = p.cfg.TimeoutTXTS

	if p.cfg.FreeRunning {
		log.Warning("operating in FreeRunning mode, will NOT adjust clock")
		p.clock = &FreeRunningClock{}
	} else {
		if p.cfg.Timestamping == timestamp.HW {
			phcDev, err := NewPHC(p.cfg.Iface)
			if err != nil {
				return err
			}
			p.clock = phcDev
		} else {
			p.clock = &SysClock{}
		}
	}

	freq, err := p.clock.FrequencyPPB()
	log.Debugf("starting PHC frequency: %v", freq)
	if err != nil {
		return err
	}

	servoCfg := servo.DefaultServoConfig()
	// update first step threshold if it's configured
	if p.cfg.FirstStepThreshold != 0 {
		// allow stepping clock on first update
		servoCfg.FirstUpdate = true
		servoCfg.FirstStepThreshold = int64(p.cfg.FirstStepThreshold)
	}
	pi := servo.NewPiServo(servoCfg, servo.DefaultPiServoCfg(), -freq)
	maxFreq, err := p.clock.MaxFreqPPB()
	if err != nil {
		log.Warningf("max PHC frequency error: %v", err)
		maxFreq = phc.DefaultMaxClockFreqPPB
	}
	pi.SetMaxFreq(maxFreq)
	log.Debugf("max PHC frequency: %v", maxFreq)
	piFilterCfg := servo.DefaultPiServoFilterCfg()
	servo.NewPiServoFilter(pi, piFilterCfg)
	p.pi = pi
	return nil
}

// ptping probing if packet is ptping before discarding it
// It's used for external pingers such as ptping and not required for sptp itself
func (p *SPTP) ptping(sourceIP netip.Addr, sourcePort int, response []byte, rxtx time.Time) error {
	// Has to be a delay request from ptping
	b := &ptp.SyncDelayReq{}
	if err := ptp.FromBytes(response, b); err != nil {
		return fmt.Errorf("failed to read DELAY_REQ %w", err)
	}
	// a probe is a bare DELAY_REQ. A SYNC, or the stamped DELAY_REQ handleDelayReq
	// sends, is another host's answer, and answering it back loops between the two
	if b.MessageType() != ptp.MessageDelayReq || !b.OriginTimestamp.Empty() {
		return fmt.Errorf("not a ptping request: %s with origin %s", b.MessageType(), b.OriginTimestamp)
	}
	// use first event connection, doesn't really matter which one we use
	c, err := NewClient(sourceIP, sourcePort, p.clockID, p.eventConns[0], p.genConn, p.cfg, p.stats)
	if err != nil {
		return fmt.Errorf("failed to respond to a DELAY_REQ %w", err)
	}
	if err := c.handleDelayReq(p.clockID, rxtx); err != nil {
		return fmt.Errorf("failed to respond to a DELAY_REQ %w", err)
	}
	return nil
}

// handlePDelayReq responds to a Pdelay_Req message with Pdelay_Resp and Pdelay_Resp_Follow_Up
// This implements the responder side of peer delay measurement for in-rack linearizability checks
func (p *SPTP) handlePDelayReq(econn UDPConnWithTS, buf []byte, addr unix.Sockaddr, rxts time.Time) error {
	if rxts.IsZero() {
		return fmt.Errorf("no receive timestamp on Pdelay_Req")
	}

	req := &ptp.PDelayReq{}
	if err := ptp.FromBytes(buf, req); err != nil {
		return fmt.Errorf("parsing Pdelay_Req: %w", err)
	}

	seq := req.SequenceID
	log.Debugf("[pdelay-responder] received Pdelay_Req seq=%d, T2=%v", seq, rxts)

	// Build and send Pdelay_Resp with T2 (request receipt timestamp)
	resp := ptp.RespPDelay(p.clockID, 1, ptp.NewTimestamp(rxts), req)
	respBytes, err := ptp.Bytes(resp)
	if err != nil {
		return fmt.Errorf("marshaling Pdelay_Resp: %w", err)
	}

	t3, err := econn.WriteToWithTS(respBytes, nil, addr, seq)
	if err != nil {
		return fmt.Errorf("sending Pdelay_Resp: %w", err)
	}

	log.Debugf("[pdelay-responder] sent Pdelay_Resp seq=%d, T3=%v", seq, t3)

	// Build and send Pdelay_Resp_Follow_Up with T3 (response origin timestamp)
	followUp := ptp.RespFollowUpPDelay(p.clockID, 1, ptp.NewTimestamp(t3), req)
	followUpBytes, err := ptp.Bytes(followUp)
	if err != nil {
		return fmt.Errorf("marshaling Pdelay_Resp_Follow_Up: %w", err)
	}

	if timestamp.SockaddrToPort(addr) == ptp.PortEvent {
		// Follow up packets have to go to General Port, not Event Port
		addr = timestamp.NewSockaddrWithPort(addr, ptp.PortGeneral)
	}
	if _, err := p.genConn.WriteTo(followUpBytes, addr); err != nil {
		return fmt.Errorf("sending Pdelay_Resp_Follow_Up: %w", err)
	}

	log.Debugf("[pdelay-responder] sent Pdelay_Resp_Follow_Up seq=%d", seq)

	return nil
}

func (p *SPTP) handlePDelayResp(buf []byte, addr netip.Addr, rxts time.Time) error {
	resp := &ptp.PDelayResp{}
	if err := ptp.FromBytes(buf, resp); err != nil {
		return fmt.Errorf("parsing Pdelay_Resp: %w", err)
	}
	if req := p.inflight(); req.matches(resp.Header, addr) {
		req.collectPDelayResp(resp, addr, rxts)
	}
	return nil
}

func (p *SPTP) handlePDelayRespFollowup(buf []byte, addr netip.Addr) error {
	resp := &ptp.PDelayRespFollowUp{}
	if err := ptp.FromBytes(buf, resp); err != nil {
		return fmt.Errorf("parsing Pdelay_Resp_Follow_Up: %w", err)
	}
	if req := p.inflight(); req.matches(resp.Header, addr) {
		req.collectPDelayRespFollowUp(resp, addr)
	}
	return nil
}

// RunListener starts a listener, must be run before any client-server interactions happen
func (p *SPTP) RunListener(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	// get announce packets from general port
	eg.Go(func() error {
		// it's done in non-blocking way, so if context is cancelled we exit correctly
		doneChan := make(chan error, 1)
		go func() {
			announce := &ptp.Announce{}
			buf := make([]byte, timestamp.PayloadSizeBytes)
			for {
				bbuf, addr, err := p.genConn.ReadPacketBuf(buf)
				if err != nil {
					doneChan <- err
					return
				}
				if !addr.IsValid() {
					doneChan <- fmt.Errorf("received packet on port 320 with invalid source address")
					return
				}

				addr = addr.Unmap()
				log.Debugf("[%s] received packet on port 320, n = %v", addr, bbuf)

				// Probe message type first
				msgType, err := ptp.ProbeMsgType(buf[:bbuf])
				if err != nil {
					log.Warningf("probing message type: %v", err)
					continue
				}

				if msgType == ptp.MessagePDelayRespFollowUp {
					if err := p.handlePDelayRespFollowup(buf[:bbuf], addr); err != nil {
						log.Warningf("[%s] handling Pdelay_Resp_FollowUp: %v", addr, err)
					}
					continue
				}

				if p.collectPingReply(buf[:bbuf], addr, time.Time{}) {
					continue
				}

				cc, found := p.clients[addr]
				if !found {
					log.Warningf("[%s] ignoring packets from unknown server", addr)
					continue
				}

				if err = ptp.FromBytes(buf[:bbuf], announce); err != nil {
					log.Warningf("[%s] reading ANNOUNCE: %v", addr, err)
					continue
				}
				cc.stats.IncRXAnnounce()
				cc.handleAnnounce(announce)
				cc.inChan <- true
			}
		}()
		select {
		case <-ctx.Done():
			log.Debug("cancelled general port receiver")
			return ctx.Err()
		case err := <-doneChan:
			return err
		}
	})
	// if we have parallel TX, we need to listen on all event ports since they all bind to same port with SO_REUSEPORT,
	// and kernel will distribute incoming packets between them evenly
	for _, econn := range p.eventConns {
		// get packets from event port
		eg.Go(func() error {
			// it's done in non-blocking way, so if context is cancelled we exit correctly
			doneChan := make(chan error, 1)
			go func() {
				sync := &ptp.SyncDelayReq{}
				buf := make([]byte, timestamp.PayloadSizeBytes)
				oob := make([]byte, timestamp.ControlSizeBytes)
				for {
					bbuf, boob, addr, err := econn.ReadPacketBuf(buf, oob)
					if err != nil {
						doneChan <- err
						return
					}
					ip := timestamp.SockaddrToAddr(addr)
					log.Debugf("[%s] received packet on port 319, n = %v", ip, bbuf)

					msgType, err := ptp.ProbeMsgType(buf[:bbuf])
					if err != nil {
						log.Warningf("probing message type: %v", err)
						continue
					}

					// only event messages are stamped, and ptping answers with an ANNOUNCE
					var rxtx time.Time
					if msgType.IsEvent() {
						if rxtx, err = econn.RXTimestamp(oob, boob); err != nil {
							// anything sent to the pdelay group arrives unstamped
							if errors.Is(err, timestamp.ErrNoTimestamp) && msgType.IsPDelay() {
								log.Warningf("received %s without timestamp, skipping: %v", msgType, err)
								continue
							}
							doneChan <- err
							return
						}
					}

					// a peer replies to the port we probed from, so the follow-up lands here too
					switch msgType {
					case ptp.MessagePDelayReq:
						if err := p.handlePDelayReq(econn, buf[:bbuf], addr, rxtx); err != nil {
							log.Warningf("[%s] handling Pdelay_Req: %v", ip, err)
						}
						continue
					case ptp.MessagePDelayResp:
						if err := p.handlePDelayResp(buf[:bbuf], ip, rxtx); err != nil {
							log.Warningf("[%s] handling Pdelay_Resp: %v", ip, err)
						}
						continue
					case ptp.MessagePDelayRespFollowUp:
						if err := p.handlePDelayRespFollowup(buf[:bbuf], ip); err != nil {
							log.Warningf("[%s] handling Pdelay_Resp_FollowUp: %v", ip, err)
						}
						continue
					}

					if p.collectPingReply(buf[:bbuf], ip, rxtx) {
						continue
					}

					cc, found := p.clients[ip]
					if !found {
						log.Warningf("[%s] ignoring packets from unknown server, trying ptping", ip)
						// Try ptping
						if err = p.ptping(ip, timestamp.SockaddrToPort(addr), buf[:bbuf], rxtx); err != nil {
							log.Warningf("[%s] ptping error: %v", ip, err)
						}
						continue
					}
					if err = ptp.FromBytes(buf[:bbuf], sync); err != nil {
						log.Warningf("[%s] reading SYNC: %v", ip, err)
						continue
					}
					cc.stats.IncRXSync()
					cc.handleSync(sync, rxtx)
					cc.inChan <- true
				}
			}()
			select {
			case <-ctx.Done():
				log.Debug("cancelled event port receiver")
				return ctx.Err()
			case err := <-doneChan:
				return err
			}
		})
	}

	return eg.Wait()
}

func (p *SPTP) handleExchangeError(addr netip.Addr, err error, tickDuration time.Duration) {
	if errors.Is(err, errBackoff) {
		b := p.backoff[addr].dec(tickDuration)
		log.Debugf("[%s] backoff %s", addr, b)
	} else {
		log.Errorf("[%s] error: %+v", addr, err)
		b := p.backoff[addr].fail(err)
		if b != 0 {
			log.Warningf("[%s] backoff extended by %s", addr, b)
		}
	}
}

// checkInterfaceDown checks if the network interface is down or has no carrier.
// It queries the current interface state using net.InterfaceByName which returns
// a fresh snapshot of the interface status from the kernel.
func checkInterfaceDown(ifaceName string) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("interface not found: %w", err)
	}

	// Check IFF_UP flag (administratively up)
	if iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("administratively down (no UP flag)")
	}

	// Check IFF_RUNNING flag (has carrier/link)
	// Running flag support: https://go-review.googlesource.com/c/go/+/413454
	if iface.Flags&net.FlagRunning == 0 {
		return fmt.Errorf("no carrier (NO-CARRIER, no RUNNING flag)")
	}

	// Interface appears healthy based on flags
	return nil
}

func (p *SPTP) setMeanFreq() (float64, error) {
	freqAdj := p.pi.MeanFreq()
	p.pi.SetLastFreq(freqAdj)
	if err := p.clock.AdjFreqPPB(-1 * freqAdj); err != nil {
		log.Errorf("failed to adjust freq to %v: %v", -freqAdj, err)
		return freqAdj, err
	}
	return freqAdj, nil
}

// reprioritize is pushing former "best gm" to the back of the list
func (p *SPTP) reprioritize(bestAddr netip.Addr) {
	// by how much we should shift the list
	k := p.priorities[bestAddr] - 1
	for addr, prio := range p.priorities {
		p.priorities[addr] = prio - k
		if p.priorities[addr] < 1 {
			// 0 -> 4, -1 -> 3...
			p.priorities[addr] = p.priorities[addr] + len(p.priorities)
		}
	}
}

// portChanges is how many times asymmetry correction has moved this GM's response
// port. The process-wide counter cannot say which GM was corrected.
// moves in the current search, not the TLV offset: the offset names the path
// the host settled on, so it never returns to zero and cannot say "done".
func (p *SPTP) portChanges(addr netip.Addr) uint16 {
	if p.corrector == nil {
		return 0
	}
	return p.corrector.PortMoves(addr)
}

// searchState asks the corrector about the same GM it was given this tick, so a
// reported state cannot disagree with the correction that produced it.
func (p *SPTP) searchState(addr netip.Addr, res *RunResult) asymmetry.SearchState {
	if p.corrector == nil {
		return asymmetry.SearchUnknown
	}
	return p.corrector.Search(addr, newGM(p.clients[addr], res))
}

func (p *SPTP) processResults(results map[netip.Addr]*RunResult) error {
	var state servo.State
	defer func() {
		for addr, res := range results {
			s := runResultToGMStats(addr, res, p.priorities[addr], addr == p.bestGM, int(state), p.portChanges(addr), p.searchState(addr, res))
			p.stats.SetGMStats(s)
		}
	}()

	isBadTick := false
	now := time.Now()
	var tickDuration time.Duration
	if !p.lastTick.IsZero() {
		tickDuration = now.Sub(p.lastTick)
		log.Debugf("tick took %vms sys time", tickDuration.Milliseconds())
		// +-10% of interval
		p.stats.SetTickDuration(tickDuration)
		if tickDuration.Seconds() > 60 {
			log.Warningf("tick took %vs, the app seems to be stalled", tickDuration.Seconds())
			p.isStalled = true
		} else if 100*tickDuration > 110*p.cfg.Interval || 100*tickDuration < 90*p.cfg.Interval {
			log.Warningf("tick took %vms, which is outside of expected +-10%% from the interval %vms", tickDuration.Milliseconds(), p.cfg.Interval.Milliseconds())
			isBadTick = true
		}
	}
	p.lastTick = now
	gmsTotal := len(results)
	gmsAvailable := 0
	idsToClients := map[ptp.ClockIdentity]netip.Addr{}
	localPrioMap := map[ptp.ClockIdentity]int{}
	for addr, res := range results {
		if res.Error != nil {
			p.handleExchangeError(addr, res.Error, tickDuration)
			continue
		}
		p.backoff[addr].reset()
		if res.Measurement == nil {
			log.Errorf("[%s] RunResult is missing MeasurementResult", addr)
			continue
		}
		log.Debugf("[%s gmid:%s] offset %v delay %v badDelay %v", addr, res.Measurement.Announce.GrandmasterIdentity, res.Measurement.Offset, res.Measurement.Delay, res.Measurement.BadDelay)
		if res.Measurement.BadDelay {
			p.stats.IncFiltered()
		}

		gmsAvailable++
		idsToClients[res.Measurement.Announce.GrandmasterIdentity] = addr
		localPrioMap[res.Measurement.Announce.GrandmasterIdentity] = p.priorities[addr]
	}
	p.stats.SetGmsTotal(gmsTotal)
	if gmsTotal != 0 {
		if gmsAvailable == 0 {
			// If no GMs are available, check the interface status
			log.Debugf("No GMs are available, checking interface %s status", p.cfg.Iface)
			if ifaceErr := checkInterfaceDown(p.cfg.Iface); ifaceErr != nil {
				log.Warningf("Interface %s is down: %v, entering holdover", p.cfg.Iface, ifaceErr)
				p.isStalled = true
			} else {
				log.Debugf("Interface %s is healthy, errors are due to other issues", p.cfg.Iface)
			}
		}
		p.stats.SetGmsAvailable(int((float64(gmsAvailable) / float64(gmsTotal)) * 100))
	} else {
		p.stats.SetGmsAvailable(0)
	}
	best := bmca(results, localPrioMap, p.cfg)
	if best == nil {
		log.Warning("no Best Master selected")
		p.bestGM = netip.Addr{}
		if p.pi.GetState() == servo.StateLocked {
			state = servo.StateHoldover
		} else {
			state = servo.StateInit
		}
		freqAdj, err := p.setMeanFreq()
		p.stats.SetServoState(int(state))
		log.Infof("[no GM] offset Unknown s%d freq %+7.0f path delay Unknown", state, -freqAdj)
		return err
	}
	bestAddr := idsToClients[best.GrandmasterIdentity]
	bm := results[bestAddr].Measurement
	if p.bestGM != bestAddr {
		p.reprioritize(bestAddr)
		log.Warningf("[%s gmid:%s] new best master selected", bestAddr, bm.Announce.GrandmasterIdentity)
		p.bestGM = bestAddr
	}
	bmOffset := int64(bm.Offset)
	bmDelay := bm.Delay.Nanoseconds()
	log.Debugf("[%s gmid:%s] best master", bestAddr, bm.Announce.GrandmasterIdentity)
	isSpike := p.pi.IsSpike(bmOffset)

	if isSpike && bm.Offset.Abs() > 77*time.Hour {
		// mitigate Broadcom 48bit overflow, in either direction
		p.isStalled = true
	}
	var freqAdj float64
	if p.isStalled {
		var err error
		if freqAdj, err = p.setMeanFreq(); err != nil {
			return err
		}
		// if we are in the stalled state, we can step the clock, but only once
		p.isStalled = false
		// if calculated offset is far away from the range - step it after stall
		if isSpike {
			state = servo.StateJump
		} else {
			state = servo.StateHoldover
		}
	} else if isSpike {
		p.stats.IncFiltered()
		var err error
		if freqAdj, err = p.setMeanFreq(); err != nil {
			return err
		}
		if p.pi.GetState() == servo.StateLocked {
			state = servo.StateFilter
		} else {
			state = servo.StateInit
		}
		results[bestAddr].Measurement = nil
	} else if isBadTick {
		var err error
		if freqAdj, err = p.setMeanFreq(); err != nil {
			return err
		}
		state = servo.StateHoldover
	} else {
		freqAdj, state = p.pi.Sample(bmOffset, uint64(bm.Timestamp.UnixNano()))
	}
	p.stats.SetServoState(int(state))
	log.Infof("[%s gmid:%s] offset %10d servo %s freq %+7.0f path delay %10d (%6d:%6d)", bestAddr, bm.Announce.GrandmasterIdentity, bmOffset, state.String(), -freqAdj, bmDelay, bm.C2SDelay, bm.S2CDelay)
	switch state {
	case servo.StateJump:
		if err := p.stepClock(-bm.Offset); err != nil {
			log.Errorf("failed to step clock by %v: %v", -bm.Offset, err)
			return err
		}
	case servo.StateLocked:
		if err := p.clock.AdjFreqPPB(-freqAdj); err != nil {
			log.Errorf("failed to adjust freq to %v: %v", -freqAdj, err)
			return err
		}
		if err := p.clock.SetSync(); err != nil {
			log.Error("failed to set clock sync state")
		}
		// make sure we don't step after we get into the locked state
		p.pi.UnsetFirstUpdate()
	}
	if p.corrector != nil && !isSpike && !isBadTick && p.pi.IsStable(bmOffset) {
		p.stats.IncPortChangeCount(p.correctAsymmetry(results, bestAddr))
	}
	return nil
}

func (p *SPTP) stepClock(step time.Duration) error {
	// a relative step is silently discarded when the PHC counter has wrapped; only an absolute set rebases it
	const absStepThreshold = time.Hour

	if step.Abs() < absStepThreshold {
		log.Infof("stepping clock by %v", step)
		return p.clock.Step(step)
	}

	before, err := p.clock.Time()
	if err != nil {
		return err
	}
	target := before.Add(step)
	log.Warningf("offset %v is beyond a relative step, setting clock to %v", step, target)
	if err := p.clock.SetTime(target); err != nil {
		return err
	}
	after, err := p.clock.Time()
	if err != nil {
		return err
	}
	if off := after.Sub(target).Abs(); off > time.Second {
		return fmt.Errorf("clock rejected absolute set, still off by %v", off)
	}
	return nil
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

func (p *SPTP) runInternal(ctx context.Context) error {
	if err := sdNotify(); err != nil {
		log.Errorf("sd_notify() failed: %v", err)
	}

	p.pi.SyncInterval(p.cfg.Interval.Seconds())
	var lock sync.Mutex

	tick := func() error {
		eg, ictx := errgroup.WithContext(ctx)
		results := map[netip.Addr]*RunResult{}
		for addr, c := range p.clients {
			if p.backoff[addr].active() {
				// skip talking to this GM, we are in backoff mode
				lock.Lock()
				results[addr] = &RunResult{
					Server: addr,
					Error:  p.backoff[addr].skipErr(),
				}
				lock.Unlock()
				continue
			}
			eg.Go(func() error {
				res := c.RunOnce(ictx, p.cfg)
				lock.Lock()
				defer lock.Unlock()
				results[addr] = res
				return nil
			})
		}
		err := eg.Wait()
		if err != nil {
			log.Errorf("run failed: %v", err)
		}
		return p.processResults(results)
	}

	timer := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			log.Debug("cancelled main loop")
			freqAdj := p.pi.MeanFreq()
			log.Infof("Existing, setting freq to: %v", -freqAdj)
			if err := p.clock.AdjFreqPPB(-1 * freqAdj); err != nil {
				log.Errorf("failed to adjust freq to %v: %v", -freqAdj, err)
			}
			return ctx.Err()
		case <-timer.C:
			timer.Reset(p.cfg.Interval)
			if err := tick(); err != nil {
				return fmt.Errorf("got unrecoverable error %w", err)
			}
		}
	}
}

// Run makes things run, continuously
func (p *SPTP) Run(ctx context.Context) error {
	go func() {
		log.Debug("starting listener")
		if err := p.RunListener(ctx); err != nil {
			log.Fatal(err)
		}
	}()
	return p.runInternal(ctx)
}
