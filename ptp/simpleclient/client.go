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

package simpleclient

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/fatih/color"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	ptp "github.com/facebook/time/ptp/protocol"
	"github.com/facebook/time/timestamp"
)

type state int

const (
	stateInit = iota
	stateInProgress
	stateDone
)

var stateToString = map[state]string{
	stateInit:       "INIT",
	stateDone:       "DONE",
	stateInProgress: "IN_PROGRESS",
}

func (s state) String() string {
	return stateToString[s]
}

// inPacket is input packet data + receive timestamp
type inPacket struct {
	data []byte
	ts   time.Time
}

// UDPConn describes what functionality we expect from UDP connection
type UDPConn interface {
	ReadFromUDP(b []byte) (int, *net.UDPAddr, error)
	WriteTo(b []byte, addr net.Addr) (int, error)
	Close() error
}

// UDPConnWithTS describes what functionality we expect from UDP connection that allows us to read TX timestamps
type UDPConnWithTS interface {
	UDPConn
	WriteToWithTS(b []byte, addr net.Addr) (int, time.Time, error)
	WriteToSrcAddrTS(b []byte, src, dst net.Addr) (time.Time, error)
}

type udpConnTS struct {
	*net.UDPConn
}

func (c *udpConnTS) WriteToWithTS(b []byte, addr net.Addr) (int, time.Time, error) {
	n, err := c.WriteTo(b, addr)
	if err != nil {
		return 0, time.Time{}, err
	}
	// get FD of the connection. Can be optimized by doing this when connection is created
	connFd, err := timestamp.ConnFd(c.UDPConn)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("failed to get conn fd udp connection: %w", err)
	}
	hwts, _, err := timestamp.ReadTXtimestamp(connFd)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("failed to get timestamp of last packet: %w", err)
	}
	return n, hwts, nil
}

func (c *udpConnTS) WriteToSrcAddrTS(_ []byte, _, _ net.Addr) (time.Time, error) {
	return time.Time{}, fmt.Errorf("not implemented")
}

// Config specifies Client run options
type Config struct {
	// address of a server to talk to
	Address string
	// interface name that we'll use to send/receive packets
	Iface string
	// timeout of whole session
	Timeout time.Duration
	// for how long we'll request unicast transmission from server
	Duration time.Duration
	// what type of typestamping to use
	Timestamping timestamp.Timestamp
}

// Client is a very simplified PTPv2 unicast client.
// Whenever it has all the data to calculate offset/delay/etc
// it will call provided callback function with `MeasurementResult`.
type Client struct {
	cfg *Config

	// state management
	// packet sequence counters
	genSequence   uint16
	eventSequence uint16
	// state enum
	state state

	// chan for received packets regardless of port
	inChan chan *inPacket
	// listening connection on port 320
	genConn UDPConn
	// listening connection on port 319
	eventConn UDPConnWithTS
	// addresses of server we'll talk to, for both 319 and 320 port
	genAddr   *net.UDPAddr
	eventAddr *net.UDPAddr
	// our clockID derived from MAC address
	clockID ptp.ClockIdentity
	// where we store timestamps
	m *measurements
	// what to do when we receive latest measurement
	callback func(*MeasurementResult)
}

// New initializes new PTPv2 unicast client
func New(cfg *Config, callback func(*MeasurementResult)) *Client {
	c := &Client{
		inChan:   make(chan *inPacket, 10),
		m:        newMeasurements(),
		cfg:      cfg,
		callback: callback,
	}
	return c
}

func (c *Client) sendGeneralMsg(p ptp.Packet) (uint16, error) {
	seq := c.genSequence
	p.SetSequence(c.genSequence)
	b, err := ptp.Bytes(p)
	if err != nil {
		return 0, err
	}
	// send packet
	_, err = c.genConn.WriteTo(b, c.genAddr)
	if err != nil {
		return 0, err
	}
	log.Debugf("sent packet via port %d to %v", ptp.PortGeneral, c.genAddr)
	c.genSequence++
	return seq, nil
}

func (c *Client) sendEventMsg(p ptp.Packet) (uint16, time.Time, error) {
	seq := c.eventSequence
	p.SetSequence(c.eventSequence)
	b, err := ptp.Bytes(p)
	if err != nil {
		return 0, time.Time{}, err
	}
	// send packet
	_, hwts, err := c.eventConn.WriteToWithTS(b, c.eventAddr)
	if err != nil {
		log.Warnf("Error sending packet with SeqID = %04x: %v", seq, err)
		return 0, time.Time{}, err
	}
	c.eventSequence++

	log.Debugf("sent packet via port %d to %v", ptp.PortEvent, c.eventAddr)
	return seq, hwts, nil
}

func (c *Client) setup(ctx context.Context, eg *errgroup.Group) error {
	iface, err := net.InterfaceByName(c.cfg.Iface)
	if err != nil {
		return err
	}

	cid, err := ptp.NewClockIdentity(iface.HardwareAddr)
	if err != nil {
		return err
	}
	log.Infof("using ClockIdentity %s, talking to %v using Two-Step Unicast PTPv2 protocol", cid, c.cfg.Address)
	c.clockID = cid

	// addresses
	// where to send to
	genAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(c.cfg.Address, fmt.Sprintf("%d", ptp.PortGeneral)))
	if err != nil {
		return err
	}
	eventAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(c.cfg.Address, fmt.Sprintf("%d", ptp.PortEvent)))
	if err != nil {
		return err
	}
	// bind to general port
	genConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("::"), Port: ptp.PortGeneral})
	if err != nil {
		return err
	}
	c.genConn = genConn
	c.genAddr = genAddr
	// bind to event port
	eventConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("::"), Port: ptp.PortEvent})
	if err != nil {
		return err
	}

	// get FD of the connection. Can be optimized by doing this when connection is created
	connFd, err := timestamp.ConnFd(eventConn)
	if err != nil {
		return err
	}

	// we need to enable HW or SW timestamps on event port
	if err := timestamp.EnableTimestamps(c.cfg.Timestamping, connFd, iface); err != nil {
		return err
	}

	// set it to blocking mode, otherwise recvmsg will just return with nothing most of the time
	if err := unix.SetNonblock(connFd, false); err != nil {
		return fmt.Errorf("failed to set event socket to blocking: %w", err)
	}
	c.eventConn = &udpConnTS{eventConn}
	c.eventAddr = eventAddr

	// get packets from general port
	eg.Go(func() error {
		// it's done in non-blocking way, so if context is cancelled we exit correctly
		doneChan := make(chan error, 1)
		go func() {
			for {
				response := make([]uint8, 1024)
				n, addr, err := genConn.ReadFromUDP(response)
				if err != nil {
					doneChan <- err
					return
				}
				log.Debugf("got packet on port 320, n = %v, addr = %v", n, addr)
				if !addr.IP.Equal(genAddr.IP) {
					log.Warningf("ignoring packets from server %v", addr)
				}
				c.inChan <- &inPacket{data: response[:n]}
			}
		}()
		select {
		case <-ctx.Done():
			log.Debug("cancelled general port receiver")
			return ctx.Err()
		case err = <-doneChan:
			return err
		}
	})
	// get packets from event port
	eg.Go(func() error {
		// it's done in non-blocking way, so if context is cancelled we exit correctly
		doneChan := make(chan error, 1)
		go func() {
			for {
				response, addr, rxtx, err := timestamp.ReadPacketWithRXTimestamp(connFd)
				if err != nil {
					doneChan <- err
					return
				}
				log.Debugf("got packet on port 319, addr = %v", addr)
				if !net.IP(addr.Addr().AsSlice()).Equal(eventAddr.IP) {
					log.Warningf("ignoring packets from server %v", addr)
				}
				c.inChan <- &inPacket{data: response, ts: rxtx}
			}
		}()
		select {
		case <-ctx.Done():
			log.Debug("cancelled event port receiver")
			return ctx.Err()
		case err = <-doneChan:
			return err
		}
	})

	return nil
}

// handleGrantUnicast handles SIGNALLING packet that grants parts of unicast transmission
func (c *Client) handleGrantUnicast(tlv *ptp.GrantUnicastTransmissionTLV) error {
	msgType := tlv.MsgTypeAndReserved.MsgType()
	c.logReceive(ptp.MessageSignaling, "unicast grant for %s", msgType)
	switch msgType {
	case ptp.MessageAnnounce:
		// we received response, no need to request more grants for Announce
		c.setState(stateInProgress)
		if tlv.DurationField == 0 {
			return fmt.Errorf("server denied us grant for %s", msgType)
		}
		// ask for sync messages
		seq, err := c.sendGeneralMsg(reqUnicast(c.clockID, c.cfg.Duration, ptp.MessageSync))
		if err != nil {
			return err
		}
		c.logSent(ptp.MessageSignaling, "for %s, seq=%d", ptp.MessageSync, seq)
	case ptp.MessageSync:
		if tlv.DurationField == 0 {
			return fmt.Errorf("server denied us grant for %s", msgType)
		}
		// ask for delay_resp messages
		seq, err := c.sendGeneralMsg(reqUnicast(c.clockID, c.cfg.Duration, ptp.MessageDelayResp))
		if err != nil {
			return err
		}
		c.logSent(ptp.MessageSignaling, "for %s, seq=%d", ptp.MessageDelayResp, seq)
	case ptp.MessageDelayResp:
		if tlv.DurationField == 0 {
			return fmt.Errorf("server denied us grant for %s", msgType)
		}
		log.Info("unicast handshake complete")
	default:
		return fmt.Errorf("got unexpected grant for %s", msgType)
	}
	return nil
}

// handleCancelUnicast handles SIGNALLING packet that marks end of unicast transmission
func (c *Client) handleCancelUnicast(tlv *ptp.CancelUnicastTransmissionTLV) error {
	c.logReceive(ptp.MessageSignaling, "unicast transmission cancelled, dying")
	seq, err := c.sendGeneralMsg(reqAckCancelUnicast(c.clockID, tlv.MsgTypeAndFlags.MsgType()))
	if err != nil {
		return err
	}
	c.logSent(ptp.MessageSignaling, "ACK CANCEL for %s, seq=%d", tlv.MsgTypeAndFlags.MsgType(), seq)
	// real client should have answered to all CANCEL messages, but we won't
	c.setState(stateDone)
	return nil
}

// handleAnnounce handles ANNOUNCE packet and records UTC offset from it's data
func (c *Client) handleAnnounce(b *ptp.Announce) error {
	c.logReceive(ptp.MessageAnnounce, "seq=%d, gmIdentity=%s, gmTimeSource=%s, stepsRemoved=%d",
		b.SequenceID, b.GrandmasterIdentity, b.TimeSource, b.StepsRemoved)
	c.m.currentUTCoffset = time.Duration(b.CurrentUTCOffset) * time.Second
	return nil
}

// handleSync handles SYNC packet and adds send timestamp to measurements
func (c *Client) handleSync(b *ptp.SyncDelayReq, ts time.Time) error {
	c.logReceive(ptp.MessageSync, "seq=%d, our ReceiveTimestamp(T2)=%v, correctionField(C1)=%v", b.SequenceID, ts, b.CorrectionField.Duration())
	c.m.addSync(b.SequenceID, ts, b.CorrectionField.Duration())
	return nil
}

// handleDelay handles DELAY packet and adds ReceiveTimestamp to measurements
func (c *Client) handleDelay(b *ptp.DelayResp) error {
	c.logReceive(ptp.MessageDelayResp, "seq=%d, server ReceiveTimestamp(T4)=%v, correctionField(C3)=%v", b.SequenceID, b.ReceiveTimestamp.Time(), b.CorrectionField.Duration())
	// store data in measurements
	c.m.addDelayResp(b.SequenceID, b.ReceiveTimestamp.Time(), b.CorrectionField.Duration())

	// do whatever needs to be done with current measurements
	res, err := c.m.latest()
	if err != nil {
		log.Warningf("failed to get measurements: %v", err)
		return nil
	}
	c.callback(res)
	return nil
}

// handleFollowUp handles FOLLOW_UP packet and sends DELAY_REQ packet
func (c *Client) handleFollowUp(b *ptp.FollowUp) error {
	c.logReceive(ptp.MessageFollowUp, "seq=%d, server PreciseOriginTimestamp(T1)=%v, correctionField(C2)=%v", b.SequenceID, b.PreciseOriginTimestamp.Time(), b.CorrectionField.Duration())
	c.m.addFollowUp(b.SequenceID, b.PreciseOriginTimestamp.Time(), b.CorrectionField.Duration())
	// ask for delay
	seq, hwts, err := c.sendEventMsg(reqDelay(c.clockID))
	if err != nil {
		return err
	}
	c.m.addDelayReq(seq, hwts)
	c.logSent(ptp.MessageDelayReq, "seq=%d, our TransmissionTimestamp(T3)=%v", seq, hwts)
	return nil
}

// dispatch handler based on msg type
func (c *Client) handleMsg(msg *inPacket) error {
	msgType, err := ptp.ProbeMsgType(msg.data)
	if err != nil {
		return err
	}
	switch msgType {
	case ptp.MessageSignaling:
		signaling := &ptp.Signaling{}
		if err := ptp.FromBytes(msg.data, signaling); err != nil {
			return fmt.Errorf("reading signaling msg: %w", err)
		}

		for _, tlv := range signaling.TLVs {
			switch v := tlv.(type) {
			case *ptp.GrantUnicastTransmissionTLV:
				if err := c.handleGrantUnicast(v); err != nil {
					return err
				}

			case *ptp.CancelUnicastTransmissionTLV:
				if err := c.handleCancelUnicast(v); err != nil {
					return err
				}
			default:
				return fmt.Errorf("got unsupported TLV type %s(%d)", tlv.Type(), tlv.Type())
			}
		}
		return nil
	case ptp.MessageAnnounce:
		announce := &ptp.Announce{}
		if err := ptp.FromBytes(msg.data, announce); err != nil {
			return fmt.Errorf("reading announce msg: %w", err)
		}
		return c.handleAnnounce(announce)
	case ptp.MessageSync:
		b := &ptp.SyncDelayReq{}
		if err := ptp.FromBytes(msg.data, b); err != nil {
			return fmt.Errorf("reading sync msg: %w", err)
		}
		return c.handleSync(b, msg.ts)
	case ptp.MessageDelayResp:
		b := &ptp.DelayResp{}
		if err := ptp.FromBytes(msg.data, b); err != nil {
			return fmt.Errorf("reading delay_resp msg: %w", err)
		}
		return c.handleDelay(b)
	case ptp.MessageFollowUp:
		b := &ptp.FollowUp{}
		if err := ptp.FromBytes(msg.data, b); err != nil {
			return fmt.Errorf("reading follow_up msg: %w", err)
		}
		return c.handleFollowUp(b)
	default:
		c.logReceive(msgType, "unsupported, ignoring")
		return nil
	}
}

// dedicated function just for logging state changes
func (c *Client) setState(s state) {
	if c.state != s {
		log.Debugf("Changing state to %s", s)
		c.state = s
	}
}

// couple of helpers to log nice lines about happening communication
func (c *Client) logSent(t ptp.MessageType, msg string, v ...any) {
	log.Info(color.GreenString("client -> %s (%s)", t, fmt.Sprintf(msg, v...)))
}
func (c *Client) logReceive(t ptp.MessageType, msg string, v ...any) {
	log.Info(color.BlueString("server -> %s (%s)", t, fmt.Sprintf(msg, v...)))
}

// Run is the main function, it makes client talk to server provided in config
func (c *Client) Run() error {
	return c.runInternal(false)
}

// runInternal allows us to skip setup for unittests
func (c *Client) runInternal(skipSetup bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	eg, ctx := errgroup.WithContext(ctx)

	if !skipSetup {
		if err := c.setup(ctx, eg); err != nil {
			return err
		}
	}

	eg.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				log.Debug("cancelled main loop")
				return ctx.Err()
			case msg := <-c.inChan:
				if err := c.handleMsg(msg); err != nil {
					return err
				}
			default:
				switch c.state {
				case stateInit:
					seq, err := c.sendGeneralMsg(reqUnicast(c.clockID, c.cfg.Duration, ptp.MessageAnnounce))
					if err != nil {
						return err
					}
					c.logSent(ptp.MessageSignaling, "for %s, seq=%d", ptp.MessageAnnounce, seq)
					time.Sleep(time.Second)
				case stateDone:
					cancel()
					return nil
				}
			}
		}
	})
	return eg.Wait()
}

// Close connections
func (c *Client) Close() {
	if c.eventConn != nil {
		c.eventConn.Close()
	}
	if c.genConn != nil {
		c.genConn.Close()
	}
}
