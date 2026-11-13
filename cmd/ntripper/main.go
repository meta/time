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

// ntripper reads RTCM3 and UBX-RXM-RAWX data from an oscillatord Unix socket
// and pushes RTCM corrections to an NTRIP caster using the SOURCE protocol.
// When RAWX observations are available, ntripper generates MSM7 messages with
// proper carrier phase from raw observations, bypassing the receiver's limited
// native RTCM engine.
//
// Usage:
//
//	ntripper -caster caster.example.com:2101 -username MyMount -password secret
//	ntripper -caster caster.example.com:2101 -username MyMount -password secret \
//	         -proxy proxy.example.com:8082 -proxy-cert /path/to/cert.pem
package main

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/facebook/time/cmd/ntripper/stats"
	"github.com/facebook/time/ntrip"
	"github.com/facebook/time/rtcm"
)

// msg1033Interval is how often the antenna descriptor is re-injected.
const msg1033Interval = 30 * time.Second

type config struct {
	dumpW             io.Writer
	socket            string
	caster            string
	mountpoint        string
	password          string
	username          string
	userAgent         string
	proxy             string
	proxyCert         string
	proxyKey          string
	logLevel          string
	dump              string
	parse             string
	reconnectInterval time.Duration
	ephInterval       time.Duration
	monitoringPort    int
	ntripVersion      int
	minCNO            uint
	dryRun            bool
	chunked           bool
	rawxStats         bool
}

func main() {
	var cfg config

	flag.StringVar(&cfg.socket, "socket", "/run/oscillatord/rtcm.sock",
		"path to the oscillatord RTCM Unix socket")
	flag.StringVar(&cfg.caster, "caster", "",
		"NTRIP caster address (host:port)")
	flag.StringVar(&cfg.mountpoint, "mountpoint", "",
		"NTRIP caster mountpoint (e.g., /MOUNT01)")
	flag.StringVar(&cfg.password, "password", "",
		"NTRIP SOURCE password")
	flag.StringVar(&cfg.username, "username", "",
		"NTRIP username (also used as mountpoint if -mountpoint not set)")
	flag.StringVar(&cfg.userAgent, "useragent", "NTRIP ntripper/1.0",
		"NTRIP source agent string")
	flag.StringVar(&cfg.proxy, "proxy", "",
		"HTTP CONNECT proxy address (host:port)")
	flag.StringVar(&cfg.proxyCert, "proxy-cert", "",
		"PEM certificate for proxy TLS authentication")
	flag.StringVar(&cfg.proxyKey, "proxy-key", "",
		"PEM private key for proxy TLS authentication (defaults to proxy-cert)")
	flag.DurationVar(&cfg.reconnectInterval, "reconnect-interval", 5*time.Second,
		"delay between reconnection attempts")
	flag.IntVar(&cfg.monitoringPort, "monitoring-port", 8891,
		"port for JSON monitoring HTTP server (0 to disable)")
	flag.StringVar(&cfg.logLevel, "log-level", "info",
		"log level (debug, info, warn, error)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false,
		"read frames from socket and print to stdout instead of pushing to caster")
	flag.StringVar(&cfg.dump, "dump", "",
		"tee the exact outgoing RTCM stream sent to the caster into this file")
	flag.StringVar(&cfg.parse, "parse", "",
		"validate the RTCM3 framing of a captured dump file and exit")
	flag.IntVar(&cfg.ntripVersion, "ntrip-version", 1,
		"NTRIP protocol version: 1 (SOURCE) or 2 (HTTP POST)")
	flag.BoolVar(&cfg.chunked, "chunked", true,
		"NTRIP v2 only: send body using HTTP chunked transfer encoding")
	flag.DurationVar(&cfg.ephInterval, "ephemeris-interval", 8*time.Second,
		"how often to re-send cached ephemeris (1019) to the caster")
	flag.UintVar(&cfg.minCNO, "min-cno", 28,
		"drop observations below this carrier-to-noise density in dB-Hz (0 disables)")
	flag.BoolVar(&cfg.rawxStats, "rawx-stats", false,
		"read the socket and report the GNSS/signal breakdown of raw RAWX observations, then exit")
	flag.Parse()

	rtcm.SetMinCNO(cfg.minCNO)

	if cfg.parse != "" {
		if err := parseStream(cfg.parse); err != nil {
			fmt.Fprintln(os.Stderr, "parse error:", err)
			os.Exit(1)
		}
		return
	}

	if !cfg.dryRun && !cfg.rawxStats && (cfg.caster == "" || cfg.password == "") {
		fmt.Fprintln(os.Stderr, "required flags: -caster, -password")
		flag.Usage()
		os.Exit(1)
	}
	if cfg.mountpoint == "" && cfg.username != "" {
		cfg.mountpoint = cfg.username
	}
	if !cfg.dryRun && !cfg.rawxStats && cfg.mountpoint == "" {
		fmt.Fprintln(os.Stderr, "required: -mountpoint or -username")
		flag.Usage()
		os.Exit(1)
	}
	if cfg.proxyKey == "" {
		cfg.proxyKey = cfg.proxyCert
	}

	logger := setupLogger(cfg.logLevel)

	if cfg.dump != "" {
		f, err := os.Create(cfg.dump)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot open dump file:", err)
			os.Exit(1)
		}
		defer f.Close()
		cfg.dumpW = f
		logger.Info("dumping outgoing RTCM stream", "path", cfg.dump)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	st := stats.NewJSONStats()
	if cfg.monitoringPort > 0 {
		go st.Start(cfg.monitoringPort)
	}

	run(ctx, cfg, logger, st)
}

// towToMs converts a RAWX receiver time of week to the millisecond MSM epoch
// field. RcvTow is a float64, so a measurement on an exact second lands a
// fraction below it (230022.0 becomes 230021999.999999); truncating stamps
// every epoch 1 ms early and pushes it into the previous second.
func towToMs(rcvTow float64) uint32 {
	return uint32(math.Mod(math.Round(rcvTow*1000.0), 604800000.0))
}

// periodicSender re-injects the antenna descriptor and cached ephemeris on
// independent cadences.
type periodicSender struct {
	lastMsg1033 time.Time
	lastEph     time.Time
	ephInterval time.Duration
}

// ephCache supplies the cached ephemeris frames to re-send.
type ephCache interface {
	All() [][]byte
}

// send writes whichever periodic messages are due and reports how many
// ephemeris frames went out.
func (p *periodicSender) send(w io.Writer, msg1033 []byte, ephColl ephCache) (uint64, error) {
	if dueNow(p.lastMsg1033, msg1033Interval) {
		if _, err := w.Write(msg1033); err != nil {
			return 0, casterWrite("1033", err)
		}
		p.lastMsg1033 = time.Now()
	}

	sent := uint64(0)
	if dueNow(p.lastEph, p.ephInterval) {
		for _, eph := range ephColl.All() {
			if _, err := w.Write(eph); err != nil {
				return sent, casterWrite("ephemeris", err)
			}
			sent++
		}
		p.lastEph = time.Now()
	}
	return sent, nil
}

// dueNow reports whether interval has elapsed since last. A non-positive
// interval disables the timer.
func dueNow(last time.Time, interval time.Duration) bool {
	return interval > 0 && time.Since(last) >= interval
}

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func run(ctx context.Context, cfg config, logger *slog.Logger, st *stats.JSONStats) {
	// The ephemeris collector persists across caster reconnects so it keeps
	// accumulating navigation subframes; a full GPS ephemeris takes ~30s to
	// assemble, longer than a single caster connection often lasts.
	ephColl := rtcm.NewEphCollector()
	for ctx.Err() == nil {
		err := runOnce(ctx, cfg, logger, st, ephColl)
		if err == nil || ctx.Err() != nil {
			return
		}

		st.SetConnected(0)
		st.IncReconnects()

		logger.Warn("connection error, reconnecting",
			"error", err,
			"interval", cfg.reconnectInterval,
		)
		if !sleep(ctx, cfg.reconnectInterval) {
			return
		}
	}
}

func runOnce(ctx context.Context, cfg config, logger *slog.Logger, st *stats.JSONStats, ephColl *rtcm.EphCollector) error {
	if cfg.dryRun {
		sockConn, err := connectSocket(ctx, cfg, logger)
		if err != nil {
			return fmt.Errorf("socket: %w", err)
		}
		defer sockConn.Close()
		return printFrames(ctx, sockConn, logger, st)
	}

	if cfg.rawxStats {
		sockConn, err := connectSocket(ctx, cfg, logger)
		if err != nil {
			return fmt.Errorf("socket: %w", err)
		}
		defer sockConn.Close()
		return rawxStats(ctx, sockConn, logger)
	}

	stationID := parseStationID(cfg.username)
	msg1033, err := rtcm.Encode1033(rtcm.AntennaDescriptor{
		StationID:      stationID,
		AntennaType:    "ADVNULLANTENNA",
		AntennaSetupID: 0,
		ReceiverType:   "u-blox F9T",
		ReceiverFW:     "2.20",
	})
	if err != nil {
		return fmt.Errorf("encoding 1033: %w", err)
	}

	client, err := connectCaster(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("caster: %w", err)
	}
	defer client.Close()

	st.SetConnected(1)

	// oscillatord closes the RTCM socket every ~5s.
	// Keep caster alive and reconnect the socket seamlessly. The ephemeris
	// collector is shared across caster reconnects (created in run).
	for ctx.Err() == nil {
		sockConn, err := connectSocket(ctx, cfg, logger)
		if err != nil {
			return fmt.Errorf("socket: %w", err)
		}

		err = streamFrames(ctx, sockConn, client, logger, st, stationID, msg1033, ephColl, cfg.ephInterval)
		sockConn.Close()

		if ctx.Err() != nil {
			return err
		}

		// Write errors mean caster is dead — need full reconnect.
		if isWriteError(err) {
			return err
		}

		// Socket EOF — just reconnect socket immediately.
		logger.Debug("socket EOF, reconnecting")
	}
	return ctx.Err()
}

// casterWriteError marks a failure writing to the caster (as opposed to reading
// the local oscillatord socket). It signals runOnce to reestablish the caster
// connection rather than just reopening the socket.
type casterWriteError struct {
	err  error
	what string
}

func (e *casterWriteError) Error() string { return "writing " + e.what + ": " + e.err.Error() }

func (e *casterWriteError) Unwrap() error { return e.err }

// casterWrite wraps an error returned while writing to the caster.
func casterWrite(what string, err error) error {
	return &casterWriteError{what: what, err: err}
}

func isWriteError(err error) bool {
	var e *casterWriteError
	return errors.As(err, &e)
}

func parseStationID(username string) uint16 {
	for i := len(username) - 1; i >= 0; i-- {
		if username[i] < '0' || username[i] > '9' {
			if i < len(username)-1 {
				n, _ := strconv.Atoi(username[i+1:])
				return uint16(n & 0xFFFF)
			}
			break
		}
	}
	return 1
}

func connectSocket(ctx context.Context, cfg config, logger *slog.Logger) (net.Conn, error) {
	logger.Info("connecting to RTCM socket", "path", cfg.socket)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", cfg.socket)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cfg.socket, err)
	}
	logger.Info("connected to RTCM socket", "path", cfg.socket)
	return conn, nil
}

func connectCaster(ctx context.Context, cfg config, logger *slog.Logger) (*ntrip.Client, error) {
	ntripCfg := ntrip.Config{
		Caster:     cfg.caster,
		Mountpoint: cfg.mountpoint,
		Password:   cfg.password,
		Username:   cfg.username,
		UserAgent:  cfg.userAgent,
		Version:    cfg.ntripVersion,
		Chunked:    cfg.chunked,
	}

	opts := []ntrip.Option{
		ntrip.WithLogger(logger),
	}

	if cfg.dumpW != nil {
		opts = append(opts, ntrip.WithDump(cfg.dumpW))
	}

	if cfg.proxy != "" {
		opts = append(opts, ntrip.WithProxy(ntrip.ProxyConfig{
			Address:  cfg.proxy,
			CertFile: cfg.proxyCert,
			KeyFile:  cfg.proxyKey,
		}))
	}

	client := ntrip.NewClient(ntripCfg, opts...)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func streamFrames(
	ctx context.Context,
	sockConn net.Conn,
	client *ntrip.Client,
	logger *slog.Logger,
	st *stats.JSONStats,
	stationID uint16,
	msg1033 []byte,
	ephColl *rtcm.EphCollector,
	ephInterval time.Duration,
) error {
	scanner := rtcm.NewMixedScanner(sockConn)
	var frameCount uint64
	var rawxCount uint64

	// Inject RTCM 1033 on connect and every 30s.
	if _, err := client.Write(msg1033); err != nil {
		return casterWrite("initial 1033", err)
	}
	logger.Info("injected RTCM 1033", "bytes", len(msg1033))

	// Send any ephemeris already collected so a freshly (re)connected caster
	// has satellite positions immediately, without waiting ~30s to reassemble.
	for _, eph := range ephColl.All() {
		if _, err := client.Write(eph); err != nil {
			return casterWrite("cached ephemeris", err)
		}
		frameCount++
	}
	periodic := &periodicSender{
		lastMsg1033: time.Now(),
		lastEph:     time.Now(),
		ephInterval: ephInterval,
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}

		switch scanner.Type() {
		case rtcm.MsgUBX:
			if scanner.UBXClass() != rtcm.UBXClassRXM {
				continue
			}
			// UBX-RXM-SFRBX: decode navigation subframes into RTCM 1019
			// (GPS ephemeris) so the caster can position the satellites.
			if scanner.UBXMsgID() == rtcm.UBXMsgSFRBX {
				if eph := ephColl.AddSFRBX(scanner.UBXPayload()); eph != nil {
					if _, err := client.Write(eph); err != nil {
						return casterWrite("ephemeris", err)
					}
					frameCount++
					logger.Info("sent GPS ephemeris (1019)", "bytes", len(eph))
				}
				continue
			}
			if scanner.UBXMsgID() != rtcm.UBXMsgRAWX {
				continue
			}

			epoch, err := rtcm.ParseRawx(scanner.UBXPayload())
			if err != nil {
				logger.Warn("RAWX parse error", "error", err)
				continue
			}

			rawxCount++
			gpsTowMs := towToMs(epoch.RcvTow)

			if rawxCount <= 5 || rawxCount%60 == 0 {
				logger.Info("RAWX epoch",
					"rawx_count", rawxCount,
					"obs", len(epoch.Observations),
					"gpsTowMs", gpsTowMs,
					"frames_sent", frameCount,
					"dropped_sats", rtcm.DroppedSats(),
					"weak_signals", rtcm.WeakSignals(),
					"unmapped_signals", rtcm.UnmappedSignals(),
					"non_finite_pseudoranges", rtcm.NonFinitePseudoranges(),
					"voided_rough_ranges", rtcm.VoidedRoughRanges(),
					"voided_rough_rates", rtcm.VoidedRoughRates(),
					"voided_fine_prs", rtcm.VoidedFinePRs(),
					"voided_fine_phases", rtcm.VoidedFinePhases(),
					"voided_fine_rates", rtcm.VoidedFineRates(),
					"receiver_flagged_phases", rtcm.ReceiverFlaggedPhases(),
					"age_s", time.Now().Unix()-(int64(315964800)+int64(epoch.Week)*604800-int64(epoch.LeapS)+int64(gpsTowMs/1000)),
				)
			}

			// Per-constellation MSM epoch time (30-bit DF004-style field):
			//   GPS/Galileo/QZSS/NavIC: GPS time of week (ms).
			//   BeiDou: BeiDou time = GPS time - 14 s.
			//   GLONASS: 3-bit day-of-week | 27-bit ms-of-day in GLONASS time
			//            (UTC + 3 h); GLONASS time = GPS time - leap seconds + 3 h.
			bdsEpoch := (gpsTowMs + 604800000 - 14000) % 604800000
			utcMs := (gpsTowMs + 604800000 - uint32(epoch.LeapS&0x7F)*1000) % 604800000
			gloMs := (utcMs + 3*3600*1000) % 604800000
			gloEpoch := (gloMs/86400000)<<27 | (gloMs % 86400000)

			// Generate MSM7 for all constellations the receiver provides. Collect
			// first so the MSM multiple-message bit (DF393) can be set on all but
			// the last message of the epoch, as the MSM spec requires.
			gens := []struct {
				gnss    uint8
				epochMs uint32
			}{
				{rtcm.GnssGPS, gpsTowMs},
				{rtcm.GnssGalileo, gpsTowMs},
				{rtcm.GnssGLONASS, gloEpoch},
				{rtcm.GnssBeiDou, bdsEpoch},
				{rtcm.GnssQZSS, gpsTowMs},
				{rtcm.GnssNavIC, gpsTowMs},
			}
			var msmFrames [][]byte
			for _, g := range gens {
				frame, err := rtcm.EncodeMSM7(stationID, g.gnss, g.epochMs, epoch.Observations)
				if errors.Is(err, rtcm.ErrNoObservations) {
					continue // constellation not in view this epoch
				}
				if err != nil {
					logger.Warn("MSM7 encode error", "gnss", g.gnss, "error", err)
					continue
				}
				// Validate frame before sending.
				if frame[0] != rtcm.Preamble {
					logger.Error("BAD FRAME: wrong preamble", "gnss", g.gnss, "byte0", frame[0])
					continue
				}
				pl := int(frame[1]&0x03)<<8 | int(frame[2])
				if pl+6 != len(frame) {
					logger.Error("BAD FRAME: length mismatch", "gnss", g.gnss, "header_len", pl, "frame_len", len(frame))
					continue
				}
				msmFrames = append(msmFrames, frame)
			}

			// Mark every MSM message except the last in this epoch with the
			// multiple-message bit, then recompute CRC.
			for i := range len(msmFrames) - 1 {
				setMSMMultipleBit(msmFrames[i])
			}

			for _, frame := range msmFrames {
				crc := rtcm.CRC24Q(frame[:len(frame)-rtcm.CRCSize])
				stored := uint32(frame[len(frame)-3])<<16 | uint32(frame[len(frame)-2])<<8 | uint32(frame[len(frame)-1])
				if crc != stored {
					logger.Error("BAD FRAME: CRC mismatch", "computed", crc, "stored", stored)
					continue
				}
				if _, err := client.Write(frame); err != nil {
					return casterWrite("MSM7", err)
				}
				frameCount++
			}

		case rtcm.MsgRTCM3:
			frame := scanner.Frame()
			st.IncFramesReceived()

			// Drop native MSM — replaced by RAWX-generated MSM7.
			if frame.MessageType >= 1071 && frame.MessageType <= 1137 {
				continue
			}

			// Forward 1005 (station position) and 1230 (GLONASS bias).
			raw := frame.Raw
			if frame.MessageType == 1005 && len(raw) >= 8 {
				raw = rtcm.Patch1005RefStation(raw)
			}
			raw = rtcm.PatchStationID(raw, stationID)

			// Validate patched frame before sending.
			pl := int(raw[1]&0x03)<<8 | int(raw[2])
			if pl+6 != len(raw) {
				logger.Error("BAD NATIVE FRAME: length mismatch", "type", frame.MessageType, "header_len", pl, "frame_len", len(raw))
				continue
			}
			crc := rtcm.CRC24Q(raw[:3+pl])
			stored := uint32(raw[len(raw)-3])<<16 | uint32(raw[len(raw)-2])<<8 | uint32(raw[len(raw)-1])
			if crc != stored {
				logger.Error("BAD NATIVE FRAME: CRC mismatch", "type", frame.MessageType)
				continue
			}

			if _, err := client.Write(raw); err != nil {
				return casterWrite("RTCM", err)
			}
			frameCount++
		}

		sent, err := periodic.send(client, msg1033, ephColl)
		frameCount += sent
		if err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading from socket: %w", err)
	}

	return fmt.Errorf("socket closed (EOF)")
}

func printFrames(ctx context.Context, sockConn net.Conn, logger *slog.Logger, st *stats.JSONStats) error {
	logger.Info("dry-run mode: printing frames to stdout")
	scanner := rtcm.NewMixedScanner(sockConn)
	var frameCount uint64

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}

		switch scanner.Type() {
		case rtcm.MsgUBX:
			if scanner.UBXClass() == rtcm.UBXClassRXM && scanner.UBXMsgID() == rtcm.UBXMsgRAWX {
				epoch, err := rtcm.ParseRawx(scanner.UBXPayload())
				if err != nil {
					fmt.Printf("RAWX parse error: %v\n", err)
					continue
				}
				fmt.Printf("RAWX: obs=%d tow=%.3f week=%d\n",
					len(epoch.Observations), epoch.RcvTow, epoch.Week)
			}
		case rtcm.MsgRTCM3:
			frame := scanner.Frame()
			frameCount++
			st.IncFramesReceived()
			fmt.Printf("frame=%d type=%d len=%d\n", frameCount, frame.MessageType, len(frame.Raw))
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading from socket: %w", err)
	}
	return fmt.Errorf("socket closed (EOF)")
}

// rawxStats reads UBX-RXM-RAWX messages from the socket and reports, per
// GNSS and signal ID, how many measurements are present and how many are
// usable (valid, nonzero pseudorange and CNR). It is a diagnostic for checking
// which signals (e.g. L1 vs L2) the receiver is actually tracking.
func rawxStats(ctx context.Context, sockConn net.Conn, logger *slog.Logger) error {
	logger.Info("rawx-stats: sampling RAWX signals from socket")
	scanner := rtcm.NewMixedScanner(sockConn)

	gnssName := map[uint8]string{0: "GPS", 1: "SBAS", 2: "GAL", 3: "BDS", 5: "QZSS", 6: "GLO", 7: "NAVIC"}
	type key struct {
		gnss uint8
		sig  uint8
	}
	total := map[key]int{}
	usable := map[key]int{}
	epochs := 0

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			break
		}
		if scanner.Type() != rtcm.MsgUBX ||
			scanner.UBXClass() != rtcm.UBXClassRXM || scanner.UBXMsgID() != rtcm.UBXMsgRAWX {
			continue
		}
		p := scanner.UBXPayload()
		if len(p) < 16 {
			continue
		}
		numMeas := int(p[11])
		if len(p) < 16+numMeas*32 {
			continue
		}
		for i := range numMeas {
			off := 16 + i*32
			prMes := math.Float64frombits(binary.LittleEndian.Uint64(p[off : off+8]))
			gnss := p[off+20]
			sig := p[off+22]
			cno := p[off+26]
			prValid := p[off+30]&0x01 != 0
			k := key{gnss, sig}
			total[k]++
			if prValid && prMes > 0 && cno > 0 {
				usable[k]++
			}
		}
		epochs++
		if epochs >= 5 {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading from socket: %w", err)
	}

	fmt.Printf("\n=== RAWX signal breakdown over %d epoch(s) ===\n", epochs)
	fmt.Printf("%-6s %-4s %-8s %-8s\n", "GNSS", "sig", "total", "usable")
	seen := map[key]bool{}
	for k := range total {
		seen[k] = true
	}
	for k := range usable {
		seen[k] = true
	}
	keys := slices.SortedFunc(maps.Keys(seen), func(a, b key) int {
		if a.gnss != b.gnss {
			return cmp.Compare(a.gnss, b.gnss)
		}
		return cmp.Compare(a.sig, b.sig)
	})
	for _, k := range keys {
		name := gnssName[k.gnss]
		if name == "" {
			name = fmt.Sprintf("g%d", k.gnss)
		}
		fmt.Printf("%-6s %-4d %-8d %-8d\n", name, k.sig, total[k], usable[k])
	}
	fmt.Printf("\nReminder: GPS sig 0=L1C/A, 3=L2CL, 4=L2CM; GAL sig 0=E1C, 1=E1B, 5=E5bI, 6=E5bQ.\n")
	fmt.Printf("If only sig 0 appears per constellation, the receiver is L1-only and L2 must be enabled in the F9T config.\n")
	return nil
}

// setMSMMultipleBit sets the MSM multiple-message bit (DF393) in an encoded MSM
// frame and recomputes the CRC. DF393 is payload bit 54 (right after the 12-bit
// message number, 12-bit station ID, and 30-bit epoch). It must be set on every
// MSM message of an epoch except the last, so a caster groups them into one
// epoch instead of treating each as a separate, conflicting epoch.
func setMSMMultipleBit(frame []byte) {
	pl := int(frame[1]&0x03)<<8 | int(frame[2])
	if len(frame) < rtcm.HeaderSize+pl+rtcm.CRCSize {
		return
	}
	// Payload bit 54 → byte 6 of payload, bit 1 from MSB.
	frame[rtcm.HeaderSize+6] |= 1 << 1
	crc := rtcm.CRC24Q(frame[:rtcm.HeaderSize+pl])
	frame[len(frame)-3] = byte((crc >> 16) & 0xFF)
	frame[len(frame)-2] = byte((crc >> 8) & 0xFF)
	frame[len(frame)-1] = byte(crc & 0xFF)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// parseStream walks a captured outgoing dump and reports whether it is a clean
// sequence of valid RTCM3 frames. Any byte that is not part of a CRC-valid
// frame is counted as a gap, which indicates stream corruption (a framing bug).
// Zero gap bytes means our framing is correct and a caster's "not in RTCM3
// format" rejection is a protocol/transport problem, not a framing one.
func parseStream(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Printf("parsing %d bytes from %s\n", len(data), path)

	pos := 0
	frames := 0
	gapBytes := 0
	tailBytes := 0
	typeCounts := map[uint16]int{}

	for pos < len(data) {
		if data[pos] != rtcm.Preamble {
			gapBytes++
			pos++
			continue
		}
		frame, err := rtcm.ParseFrame(data[pos:])
		if err != nil {
			// A 0xD3 that does not start a complete CRC-valid frame. If the
			// remaining bytes are too few, it is just a truncated tail.
			if errors.Is(err, rtcm.ErrFrameTooShort) {
				tailBytes = len(data) - pos
				break
			}
			gapBytes++
			pos++
			continue
		}
		frames++
		typeCounts[frame.MessageType]++
		pos += len(frame.Raw)
	}

	fmt.Printf("valid frames=%d gap_bytes=%d tail_bytes=%d\n", frames, gapBytes, tailBytes)
	for t, c := range typeCounts {
		fmt.Printf("  type %d: %d frames\n", t, c)
	}
	switch {
	case gapBytes > 0:
		fmt.Printf("RESULT: FRAMING BUG — %d bytes are not part of any valid RTCM3 frame\n", gapBytes)
	case frames == 0:
		fmt.Printf("RESULT: no RTCM3 frames found\n")
	default:
		fmt.Printf("RESULT: CLEAN — every byte belongs to a valid RTCM3 frame; " +
			"a caster rejection is protocol/transport, not framing\n")
	}
	return nil
}
