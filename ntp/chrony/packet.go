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

package chrony

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// original C++ versions of those consts/structs
// are in https://github.com/mlichvar/chrony/blob/master/candm.h

// ReplyType identifies reply packet type
type ReplyType uint16

// CommandType identifies command type in both request and repy
type CommandType uint16

// ModeType identifies source (peer) mode
type ModeType uint16

// SourceStateType identifies source (peer) state
type SourceStateType uint16

// ResponseStatusType identifies response status
type ResponseStatusType uint16

// PacketType - request or reply
type PacketType uint8

// AuthModeType identifies the authentication mechanism of a source
// (RPY_AD_MD_* in chrony's candm.h)
type AuthModeType uint16

// we implement latest (at the moment) protocol version
const protoVersionNumber uint8 = 6
const maxDataLen = 396

// packet types
const (
	pktTypeCmdRequest PacketType = 1
	pktTypeCmdReply   PacketType = 2
)

// authentication modes reported by 'authdata'
const (
	AuthModeNone      AuthModeType = 0
	AuthModeSymmetric AuthModeType = 1
	AuthModeNTS       AuthModeType = 2
)

// AuthDataNoKeyExchange is AuthData.LastKEAgo (chronyd's -1) when no NTS key
// exchange succeeded yet or the source doesn't use NTS
const AuthDataNoKeyExchange uint32 = 0xffffffff

// AuthModeTypeDesc provides mapping from AuthModeType to string
var AuthModeTypeDesc = [3]string{
	"none",
	"symmetric",
	"nts",
}

func (t PacketType) String() string {
	switch t {
	case pktTypeCmdRequest:
		return "request"
	case pktTypeCmdReply:
		return "reply"
	default:
		return fmt.Sprintf("unknown (%d)", t)
	}
}

// request types. Only those we support, there are more
const (
	reqNSources      CommandType = 14
	reqSourceData    CommandType = 15
	reqTracking      CommandType = 33
	reqSourceStats   CommandType = 34
	reqActivity      CommandType = 44
	reqServerStats   CommandType = 54
	reqNTPData       CommandType = 57
	reqNTPSourceName CommandType = 65
	reqAuthData      CommandType = 67
	reqSelectData    CommandType = 69

	reqClientAccessesByIndex3 CommandType = 68
)

// reply types
const (
	RpyNSources      ReplyType = 2
	RpySourceData    ReplyType = 3
	RpyTracking      ReplyType = 5
	RpySourceStats   ReplyType = 6
	RpyActivity      ReplyType = 12
	RpyServerStats   ReplyType = 14
	RpyNTPData       ReplyType = 16
	RpyNTPSourceName ReplyType = 19
	RpyAuthData      ReplyType = 20
	RpyServerStats2  ReplyType = 22
	RpySelectData    ReplyType = 23
	RpyServerStats3  ReplyType = 24
	RpyServerStats4  ReplyType = 25
	RpyNTPData2      ReplyType = 26

	RpyClientAccessesByIndex3 ReplyType = 21
)

// MaxClientAccessesByIndex is the fixed number of client slots returned per
// REQ_CLIENT_ACCESSES_BY_INDEX reply (MAX_CLIENT_ACCESSES in chrony's candm.h).
// Use it to size pagination loops over chronyd's client table.
const MaxClientAccessesByIndex = 8

// source modes
const (
	SourceModeClient ModeType = 0
	SourceModePeer   ModeType = 1
	SourceModeRef    ModeType = 2
)

// source state
const (
	SourceStateSync        SourceStateType = 0
	SourceStateUnreach     SourceStateType = 1
	SourceStateFalseTicker SourceStateType = 2
	SourceStateJittery     SourceStateType = 3
	SourceStateCandidate   SourceStateType = 4
	SourceStateOutlier     SourceStateType = 5
)

// source data flags
const (
	FlagNoselect uint16 = 0x1
	FlagPrefer   uint16 = 0x2
	FlagTrust    uint16 = 0x4
	FlagRequire  uint16 = 0x8
)

// select data flags
const (
	FlagSDOptionNoSelect uint16 = 0x1
	FlagSDOptionPrefer   uint16 = 0x2
	FlagSDOptionTrust    uint16 = 0x4
	FlagSDOptionRequire  uint16 = 0x8
)

// ntpdata flags
const (
	NTPFlagsTests        uint16 = 0x3ff
	NTPFlagInterleaved   uint16 = 0x4000
	NTPFlagAuthenticated uint16 = 0x8000
)

// response status codes
const (
	sttSuccess            ResponseStatusType = 0
	sttFailed             ResponseStatusType = 1
	sttUnauth             ResponseStatusType = 2
	sttInvalid            ResponseStatusType = 3
	sttNoSuchSource       ResponseStatusType = 4
	sttInvalidTS          ResponseStatusType = 5
	sttNotEnabled         ResponseStatusType = 6
	sttBadSubnet          ResponseStatusType = 7
	sttAccessAllowed      ResponseStatusType = 8
	sttAccessDenied       ResponseStatusType = 9
	sttNoHostAccess       ResponseStatusType = 10
	sttSourceAlreadyKnown ResponseStatusType = 11
	sttTooManySources     ResponseStatusType = 12
	sttNoRTC              ResponseStatusType = 13
	sttBadRTCFile         ResponseStatusType = 14
	sttInactive           ResponseStatusType = 15
	sttBadSample          ResponseStatusType = 16
	sttInvalidAF          ResponseStatusType = 17
	sttBadPktVersion      ResponseStatusType = 18
	sttBadPktLength       ResponseStatusType = 19
)

// StatusDesc provides mapping from ResponseStatusType to string
var StatusDesc = [20]string{
	"SUCCESS",
	"FAILED",
	"UNAUTH",
	"INVALID",
	"NOSUCHSOURCE",
	"INVALIDTS",
	"NOTENABLED",
	"BADSUBNET",
	"ACCESSALLOWED",
	"ACCESSDENIED",
	"NOHOSTACCESS",
	"SOURCEALREADYKNOWN",
	"TOOMANYSOURCES",
	"NORTC",
	"BADRTCFILE",
	"INACTIVE",
	"BADSAMPLE",
	"INVALIDAF",
	"BADPKTVERSION",
	"BADPKTLENGTH",
}

func (r ResponseStatusType) String() string {
	if int(r) >= len(StatusDesc) {
		return fmt.Sprintf("UNKNOWN (%d)", r)
	}
	return StatusDesc[r]
}

// SourceStateDesc provides mapping from SourceStateType to string
var SourceStateDesc = [6]string{
	"sync",
	"unreach",
	"falseticker",
	"jittery",
	"candidate",
	"outlier",
}

func (s SourceStateType) String() string {
	if int(s) >= len(SourceStateDesc) {
		return fmt.Sprintf("unknown (%d)", s)
	}
	return SourceStateDesc[s]
}

// ModeTypeDesc provides mapping from ModeType to string
var ModeTypeDesc = [3]string{
	"client",
	"peer",
	"reference clock",
}

func (m ModeType) String() string {
	if int(m) >= len(ModeTypeDesc) {
		return fmt.Sprintf("unknown (%d)", m)
	}
	return ModeTypeDesc[m]
}

func (m AuthModeType) String() string {
	if int(m) >= len(AuthModeTypeDesc) {
		return fmt.Sprintf("unknown (%d)", m)
	}
	return AuthModeTypeDesc[m]
}

// RequestHead is the first (common) part of the request,
// in a format that can be directly passed to binary.Write
type RequestHead struct {
	Version  uint8
	PKTType  PacketType
	Res1     uint8
	Res2     uint8
	Command  CommandType
	Attempt  uint16
	Sequence uint32
	Pad1     uint32
	Pad2     uint32
}

// GetCommand returns request packet command
func (r *RequestHead) GetCommand() CommandType {
	return r.Command
}

// SetSequence sets request packet sequence number
func (r *RequestHead) SetSequence(n uint32) {
	r.Sequence = n
}

// RequestPacket is an interface to abstract all different outgoing packets
type RequestPacket interface {
	GetCommand() CommandType
	SetSequence(n uint32)
}

// ResponsePacket is an interface to abstract all different incoming packets
type ResponsePacket interface {
	GetCommand() CommandType
	GetType() ReplyType
	GetStatus() ResponseStatusType
}

// RequestSources - packet to request number of sources (peers)
type RequestSources struct {
	RequestHead
	// we actually need this to send proper packet
	data [maxDataLen]uint8
}

// RequestSourceData - packet to request source data for source id
type RequestSourceData struct {
	RequestHead
	Index int32
	EOR   int32
	// we pass i32 - 4 bytes
	data [maxDataLen - 4]uint8
}

// RequestNTPData - packet to request NTP data for peer IP.
// As of now, it's only allowed by Chrony over unix socket connection.
type RequestNTPData struct {
	RequestHead
	IPAddr IPAddr
	EOR    int32
	// we pass at max ipv6 addr - 16 bytes
	data [maxDataLen - 16]uint8
}

// RequestNTPSourceName - packet to request source name for peer IP.
type RequestNTPSourceName struct {
	RequestHead
	IPAddr IPAddr
	EOR    int32
	// we pass at max ipv6 addr - 16 bytes
	data [maxDataLen - 16]uint8
}

// RequestAuthData - packet to request authentication data for peer IP.
// Mirrors REQ_AuthData in chrony's candm.h.
type RequestAuthData struct {
	RequestHead
	IPAddr IPAddr
	EOR    int32
	// we pass at max ipv6 addr - 16 bytes
	data [maxDataLen - 16]uint8
}

// RequestServerStats - packet to request server stats
type RequestServerStats struct {
	RequestHead
	// we actually need this to send proper packet
	data [maxDataLen]uint8
}

// RequestTracking - packet to request 'tracking' data
type RequestTracking struct {
	RequestHead
	// we actually need this to send proper packet
	data [maxDataLen]uint8
}

// RequestSourceStats - packet to request 'sourcestats' data for source id
type RequestSourceStats struct {
	RequestHead
	Index int32
	EOR   int32
	// we pass i32 - 4 bytes
	data [maxDataLen - 4]uint8
}

// RequestActivity - packet to request 'activity' data
type RequestActivity struct {
	RequestHead
	// we actually need this to send proper packet
	data [maxDataLen]uint8
}

// RequestSelectData - packet to request 'selectdata' data
type RequestSelectData struct {
	RequestHead
	Index int32
	EOR   int32
	// we pass i32 - 4 bytes
	data [maxDataLen - 4]uint8
}

// RequestClientAccessesByIndex - packet to request 'clients' data: a page of
// up to MaxClientAccessesByIndex per-client entries starting at FirstIndex.
// Mirrors REQ_ClientAccessesByIndex in chrony's candm.h.
//
// The trailing padding brings the total request size to 520 bytes, which is
// the minimum chronyd accepts for this command: chrony's PKL_CommandLength
// (pktlength.c) adds anti-amplification padding so the request is at least
// as large as the reply, and the v3 'clients' reply is 520 bytes. Smaller
// requests are rejected with BADPKTLENGTH.
type RequestClientAccessesByIndex struct {
	RequestHead
	FirstIndex uint32
	NClients   uint32
	MinHits    uint32
	Reset      uint32
	EOR        int32
	data       [480]uint8
}

// ReplyHead is the first (common) part of the reply packet,
// in a format that can be directly passed to binary.Read
type ReplyHead struct {
	Version  uint8
	PKTType  PacketType
	Res1     uint8
	Res2     uint8
	Command  CommandType
	Reply    ReplyType
	Status   ResponseStatusType
	Pad1     uint16
	Pad2     uint16
	Pad3     uint16
	Sequence uint32
	Pad4     uint32
	Pad5     uint32
}

// GetCommand returns reply packet command
func (r *ReplyHead) GetCommand() CommandType {
	return r.Command
}

// GetType returns reply packet type
func (r *ReplyHead) GetType() ReplyType {
	return r.Reply
}

// GetStatus returns reply packet status
func (r *ReplyHead) GetStatus() ResponseStatusType {
	return r.Status
}

type replySourcesContent struct {
	NSources uint32
}

// ReplySources is a usable version of a reply to 'sources' command
type ReplySources struct {
	ReplyHead
	NSources int
}

type replySourceDataContent struct {
	IPAddr         IPAddr
	Poll           int16
	Stratum        uint16
	State          SourceStateType
	Mode           ModeType
	Flags          uint16
	Reachability   uint16
	SinceSample    uint32
	OrigLatestMeas chronyFloat
	LatestMeas     chronyFloat
	LatestMeasErr  chronyFloat
}

// SourceData contains parsed version of 'source data' reply
type SourceData struct {
	IPAddr         *IPAddr
	Poll           int16
	Stratum        uint16
	State          SourceStateType
	Mode           ModeType
	Flags          uint16
	Reachability   uint16
	SinceSample    uint32
	OrigLatestMeas float64
	LatestMeas     float64
	LatestMeasErr  float64
}

func newSourceData(r *replySourceDataContent) *SourceData {
	return &SourceData{
		IPAddr:         &r.IPAddr,
		Poll:           r.Poll,
		Stratum:        r.Stratum,
		State:          r.State,
		Mode:           r.Mode,
		Flags:          r.Flags,
		Reachability:   r.Reachability,
		SinceSample:    r.SinceSample,
		OrigLatestMeas: r.OrigLatestMeas.ToFloat(),
		LatestMeas:     r.LatestMeas.ToFloat(),
		LatestMeasErr:  r.LatestMeasErr.ToFloat(),
	}
}

// ReplySourceData is a usable version of 'source data' reply for given source id
type ReplySourceData struct {
	ReplyHead
	SourceData
}

type replyTrackingContent struct {
	RefID              uint32
	IPAddr             IPAddr // our current sync source
	Stratum            uint16
	LeapStatus         uint16
	RefTime            timeSpec
	CurrentCorrection  chronyFloat
	LastOffset         chronyFloat
	RMSOffset          chronyFloat
	FreqPPM            chronyFloat
	ResidFreqPPM       chronyFloat
	SkewPPM            chronyFloat
	RootDelay          chronyFloat
	RootDispersion     chronyFloat
	LastUpdateInterval chronyFloat
}

// Tracking contains parsed version of 'tracking' reply
type Tracking struct {
	RefID              uint32
	IPAddr             net.IP
	Stratum            uint16
	LeapStatus         uint16
	RefTime            time.Time
	CurrentCorrection  float64
	LastOffset         float64
	RMSOffset          float64
	FreqPPM            float64
	ResidFreqPPM       float64
	SkewPPM            float64
	RootDelay          float64
	RootDispersion     float64
	LastUpdateInterval float64
}

func newTracking(r *replyTrackingContent) *Tracking {
	return &Tracking{
		RefID:              r.RefID,
		IPAddr:             r.IPAddr.ToNetIP(),
		Stratum:            r.Stratum,
		LeapStatus:         r.LeapStatus,
		RefTime:            r.RefTime.ToTime(),
		CurrentCorrection:  r.CurrentCorrection.ToFloat(),
		LastOffset:         r.LastOffset.ToFloat(),
		RMSOffset:          r.RMSOffset.ToFloat(),
		FreqPPM:            r.FreqPPM.ToFloat(),
		ResidFreqPPM:       r.ResidFreqPPM.ToFloat(),
		SkewPPM:            r.SkewPPM.ToFloat(),
		RootDelay:          r.RootDelay.ToFloat(),
		RootDispersion:     r.RootDispersion.ToFloat(),
		LastUpdateInterval: r.LastUpdateInterval.ToFloat(),
	}
}

// ReplyTracking has usable 'tracking' response
type ReplyTracking struct {
	ReplyHead
	Tracking
}

type replySourceStatsContent struct {
	RefID              uint32
	IPAddr             IPAddr
	NSamples           uint32
	NRuns              uint32
	SpanSeconds        uint32
	StandardDeviation  chronyFloat
	ResidFreqPPM       chronyFloat
	SkewPPM            chronyFloat
	EstimatedOffset    chronyFloat
	EstimatedOffsetErr chronyFloat
}

// SourceStats contains stats about the source
type SourceStats struct {
	RefID              uint32
	IPAddr             *IPAddr
	NSamples           uint32
	NRuns              uint32
	SpanSeconds        uint32
	StandardDeviation  float64
	ResidFreqPPM       float64
	SkewPPM            float64
	EstimatedOffset    float64
	EstimatedOffsetErr float64
}

func newSourceStats(r *replySourceStatsContent) *SourceStats {
	return &SourceStats{
		RefID:              r.RefID,
		IPAddr:             &r.IPAddr,
		NSamples:           r.NSamples,
		NRuns:              r.NRuns,
		SpanSeconds:        r.SpanSeconds,
		StandardDeviation:  r.StandardDeviation.ToFloat(),
		ResidFreqPPM:       r.ResidFreqPPM.ToFloat(),
		SkewPPM:            r.SkewPPM.ToFloat(),
		EstimatedOffset:    r.EstimatedOffset.ToFloat(),
		EstimatedOffsetErr: r.EstimatedOffsetErr.ToFloat(),
	}
}

// ReplySourceStats has usable 'sourcestats' response
type ReplySourceStats struct {
	ReplyHead
	SourceStats
}

type replyNTPDataContent struct {
	RemoteAddr      IPAddr
	LocalAddr       IPAddr
	RemotePort      uint16
	Leap            uint8
	Version         uint8
	Mode            uint8
	Stratum         uint8
	Poll            int8
	Precision       int8
	RootDelay       chronyFloat
	RootDispersion  chronyFloat
	RefID           uint32
	RefTime         timeSpec
	Offset          chronyFloat
	PeerDelay       chronyFloat
	PeerDispersion  chronyFloat
	ResponseTime    chronyFloat
	JitterAsymmetry chronyFloat
	Flags           uint16
	TXTssChar       uint8
	RXTssChar       uint8
	TotalTXCount    uint32
	TotalRXCount    uint32
	TotalValidCount uint32
	Reserved        [4]uint32
}

// NTPData contains parsed version of 'ntpdata' reply
type NTPData struct {
	RemoteAddr      net.IP
	LocalAddr       net.IP
	RemotePort      uint16
	Leap            uint8
	Version         uint8
	Mode            uint8
	Stratum         uint8
	Poll            int8
	Precision       int8
	RootDelay       float64
	RootDispersion  float64
	RefID           uint32
	RefTime         time.Time
	Offset          float64
	PeerDelay       float64
	PeerDispersion  float64
	ResponseTime    float64
	JitterAsymmetry float64
	Flags           uint16
	TXTssChar       uint8
	RXTssChar       uint8
	TotalTXCount    uint32
	TotalRXCount    uint32
	TotalValidCount uint32
}

func newNTPData(r *replyNTPDataContent) *NTPData {
	return &NTPData{
		RemoteAddr:      r.RemoteAddr.ToNetIP(),
		LocalAddr:       r.LocalAddr.ToNetIP(),
		RemotePort:      r.RemotePort,
		Leap:            r.Leap,
		Version:         r.Version,
		Mode:            r.Mode,
		Stratum:         r.Stratum,
		Poll:            r.Poll,
		Precision:       r.Precision,
		RootDelay:       r.RootDelay.ToFloat(),
		RootDispersion:  r.RootDispersion.ToFloat(),
		RefID:           r.RefID,
		RefTime:         r.RefTime.ToTime(),
		Offset:          r.Offset.ToFloat(),
		PeerDelay:       r.PeerDelay.ToFloat(),
		PeerDispersion:  r.PeerDispersion.ToFloat(),
		ResponseTime:    r.ResponseTime.ToFloat(),
		JitterAsymmetry: r.JitterAsymmetry.ToFloat(),
		Flags:           r.Flags,
		TXTssChar:       r.TXTssChar,
		RXTssChar:       r.RXTssChar,
		TotalTXCount:    r.TotalTXCount,
		TotalRXCount:    r.TotalRXCount,
		TotalValidCount: r.TotalValidCount,
	}
}

// ReplyNTPData is a what end user will get in 'ntp data' response
type ReplyNTPData struct {
	ReplyHead
	NTPData
}

type replyNTPData2Content struct {
	RemoteAddr      IPAddr
	LocalAddr       IPAddr
	RemotePort      uint16
	Leap            uint8
	Version         uint8
	Mode            uint8
	Stratum         uint8
	Poll            int8
	Precision       int8
	RootDelay       chronyFloat
	RootDispersion  chronyFloat
	RefID           uint32
	RefTime         timeSpec
	Offset          chronyFloat
	PeerDelay       chronyFloat
	PeerDispersion  chronyFloat
	ResponseTime    chronyFloat
	JitterAsymmetry chronyFloat
	Flags           uint16
	TXTssChar       uint8
	RXTssChar       uint8
	TotalTXCount    uint32
	TotalRXCount    uint32
	TotalValidCount uint32
	TotalKernelTXts uint32
	TotalKernelRXts uint32
	TotalHWTXts     uint32
	TotalHWRXts     uint32
	Reserved        [4]int32
}

// NTPData2 contains parsed version of a new 'ntpdata' reply
type NTPData2 struct {
	NTPData

	TotalKernelTXts uint32
	TotalKernelRXts uint32
	TotalHWTXts     uint32
	TotalHWRXts     uint32
}

func newNTPData2(r *replyNTPData2Content) *NTPData2 {
	return &NTPData2{
		NTPData: NTPData{
			RemoteAddr:      r.RemoteAddr.ToNetIP(),
			LocalAddr:       r.LocalAddr.ToNetIP(),
			RemotePort:      r.RemotePort,
			Leap:            r.Leap,
			Version:         r.Version,
			Mode:            r.Mode,
			Stratum:         r.Stratum,
			Poll:            r.Poll,
			Precision:       r.Precision,
			RootDelay:       r.RootDelay.ToFloat(),
			RootDispersion:  r.RootDispersion.ToFloat(),
			RefID:           r.RefID,
			RefTime:         r.RefTime.ToTime(),
			Offset:          r.Offset.ToFloat(),
			PeerDelay:       r.PeerDelay.ToFloat(),
			PeerDispersion:  r.PeerDispersion.ToFloat(),
			ResponseTime:    r.ResponseTime.ToFloat(),
			JitterAsymmetry: r.JitterAsymmetry.ToFloat(),
			Flags:           r.Flags,
			TXTssChar:       r.TXTssChar,
			RXTssChar:       r.RXTssChar,
			TotalTXCount:    r.TotalTXCount,
			TotalRXCount:    r.TotalRXCount,
			TotalValidCount: r.TotalValidCount,
		},
		TotalKernelTXts: r.TotalKernelTXts,
		TotalKernelRXts: r.TotalKernelRXts,
		TotalHWTXts:     r.TotalHWTXts,
		TotalHWRXts:     r.TotalHWRXts,
	}
}

// ReplyNTPData2 is a what end user will get in 'ntp data' response
type ReplyNTPData2 struct {
	ReplyHead
	NTPData2
}

type replyNTPSourceNameContent struct {
	Name [256]uint8
}

// NTPSourceName contains parsed version of 'sourcename' reply
type NTPSourceName struct {
	Name string
}

func newNTPSourceName(r *replyNTPSourceNameContent) *NTPSourceName {
	// Find the first null terminator to avoid reading garbage
	nullIndex := bytes.IndexByte(r.Name[:], 0)
	if nullIndex >= 0 {
		return &NTPSourceName{
			Name: string(r.Name[:nullIndex]),
		}
	}

	return &NTPSourceName{
		Name: string(r.Name[:]),
	}
}

// ReplyNTPSourceName is a what end user will get in 'sourcename' response
type ReplyNTPSourceName struct {
	ReplyHead
	NTPSourceName
}

// Activity contains parsed version of 'activity' reply
type Activity struct {
	Online       int32
	Offline      int32
	BurstOnline  int32
	BurstOffline int32
	Unresolved   int32
}

// ReplyActivity is a usable version of 'activity' response
type ReplyActivity struct {
	ReplyHead
	Activity
}

// ServerStats contains parsed version of 'serverstats' reply
type ServerStats struct {
	NTPHits  uint32
	CMDHits  uint32
	NTPDrops uint32
	CMDDrops uint32
	LogDrops uint32
}

// ReplyServerStats is a usable version of 'serverstats' response
type ReplyServerStats struct {
	ReplyHead
	ServerStats
}

// ServerStats2 contains parsed version of 'serverstats2' reply
type ServerStats2 struct {
	NTPHits     uint32
	NKEHits     uint32
	CMDHits     uint32
	NTPDrops    uint32
	NKEDrops    uint32
	CMDDrops    uint32
	LogDrops    uint32
	NTPAuthHits uint32
}

// ReplyServerStats2 is a usable version of 'serverstats2' response
type ReplyServerStats2 struct {
	ReplyHead
	ServerStats2
}

// ServerStats3 contains parsed version of 'serverstats3' reply
type ServerStats3 struct {
	NTPHits            uint32
	NKEHits            uint32
	CMDHits            uint32
	NTPDrops           uint32
	NKEDrops           uint32
	CMDDrops           uint32
	LogDrops           uint32
	NTPAuthHits        uint32
	NTPInterleavedHits uint32
	NTPTimestamps      uint32
	NTPSpanSeconds     uint32
}

// ReplyServerStats3 is a usable version of 'serverstats3' response
type ReplyServerStats3 struct {
	ReplyHead
	ServerStats3
}

// ServerStats4 contains parsed version of 'serverstats4' reply
type ServerStats4 struct {
	NTPHits               uint64
	NKEHits               uint64
	CMDHits               uint64
	NTPDrops              uint64
	NKEDrops              uint64
	CMDDrops              uint64
	LogDrops              uint64
	NTPAuthHits           uint64
	NTPInterleavedHits    uint64
	NTPTimestamps         uint64
	NTPSpanSeconds        uint64
	NTPDaemonRxtimestamps uint64
	NTPDaemonTxtimestamps uint64
	NTPKernelRxtimestamps uint64
	NTPKernelTxtimestamps uint64
	NTPHwRxTimestamps     uint64
	NTPHwTxTimestamps     uint64
}

// ReplyServerStats4 is a usable version of 'serverstats4' response
type ReplyServerStats4 struct {
	ReplyHead
	ServerStats4
}

type replySelectData struct {
	RefID          uint32
	IPAddr         IPAddr
	StateChar      uint8
	Authentication uint8
	Leap           uint8
	Pad            uint8
	ConfOptions    uint16
	EFFOptions     uint16
	LastSampleAgo  uint32
	Score          chronyFloat
	LoLimit        chronyFloat
	HiLimit        chronyFloat
}

// SelectData contains parsed version of 'selectdata' reply
type SelectData struct {
	RefID          uint32
	IPAddr         net.IP
	StateChar      uint8
	Authentication uint8
	Leap           uint8
	ConfOptions    uint16
	EFFOptions     uint16
	LastSampleAgo  uint32
	Score          float64
	LoLimit        float64
	HiLimit        float64
}

// ReplySelectData is a usable version of 'selectdata' response
type ReplySelectData struct {
	ReplyHead
	SelectData
}

func newSelectData(r *replySelectData) *SelectData {
	return &SelectData{
		RefID:          r.RefID,
		IPAddr:         r.IPAddr.ToNetIP(),
		StateChar:      r.StateChar,
		Authentication: r.Authentication,
		Leap:           r.Leap,
		ConfOptions:    r.ConfOptions,
		EFFOptions:     r.EFFOptions,
		LastSampleAgo:  r.LastSampleAgo,
		Score:          r.Score.ToFloat(),
		LoLimit:        r.LoLimit.ToFloat(),
		HiLimit:        r.HiLimit.ToFloat(),
	}
}

// replyAuthDataContent is the wire-format reply, matching RPY_AuthData
// in chrony's candm.h (chronyd doesn't send the trailing EOR field)
type replyAuthDataContent struct {
	Mode         AuthModeType
	KeyType      uint16
	KeyID        uint32
	KeyLength    uint16
	KEAttempts   uint16
	LastKEAgo    uint32
	Cookies      uint16
	CookieLength uint16
	NAK          uint16
	Pad          uint16
}

// AuthData contains parsed version of 'authdata' reply
type AuthData struct {
	Mode         AuthModeType
	KeyType      uint16 // hash/cipher id (symmetric) or IANA AEAD id (NTS)
	KeyID        uint32 // key number (symmetric), NTS-KE context id (NTS)
	KeyLength    uint16 // bits, server-to-client key for NTS
	KEAttempts   uint16 // NTS-KE attempts since the last successful one
	LastKEAgo    uint32 // seconds since the last successful NTS-KE, AuthDataNoKeyExchange if none
	Cookies      uint16 // NTS cookies held
	CookieLength uint16 // bytes, next NTS cookie
	NAK          bool   // last NTS reply was a NAK
}

// ReplyAuthData is a usable version of 'authdata' response
type ReplyAuthData struct {
	ReplyHead
	AuthData
}

func newAuthData(r *replyAuthDataContent) *AuthData {
	return &AuthData{
		Mode:         r.Mode,
		KeyType:      r.KeyType,
		KeyID:        r.KeyID,
		KeyLength:    r.KeyLength,
		KEAttempts:   r.KEAttempts,
		LastKEAgo:    r.LastKEAgo,
		Cookies:      r.Cookies,
		CookieLength: r.CookieLength,
		NAK:          r.NAK != 0,
	}
}

// replyClientAccessesByIndexClient is the wire-format per-client entry,
// matching RPY_ClientAccesses_Client in chrony's candm.h. 60 bytes.
type replyClientAccessesByIndexClient struct {
	IPAddr             IPAddr
	NTPHits            uint32
	NKEHits            uint32
	CmdHits            uint32
	NTPDrops           uint32
	NKEDrops           uint32
	CmdDrops           uint32
	NTPInterval        int8
	NKEInterval        int8
	CmdInterval        int8
	NTPTimeoutInterval int8
	LastNTPHitAgo      uint32
	LastNKEHitAgo      uint32
	LastCmdHitAgo      uint32
}

// replyClientAccessesByIndexContent is the wire-format reply, matching
// RPY_ClientAccessesByIndex in chrony's candm.h. The clients array is fixed
// at MaxClientAccessesByIndex slots; NClients tells you how many are valid.
type replyClientAccessesByIndexContent struct {
	NIndices  uint32
	NextIndex uint32
	NClients  uint32
	Clients   [MaxClientAccessesByIndex]replyClientAccessesByIndexClient
}

// ClientAccess contains parsed per-client statistics from chronyd's
// 'clients' command. IPAddr is the raw chrony IPAddr; use ToNetIP for a
// resolved net.IP or String for chronyc-style output (which also handles
// unresolved entries as "ID#XXXXXXXXXX").
type ClientAccess struct {
	IPAddr             *IPAddr
	NTPHits            uint32
	NKEHits            uint32
	CmdHits            uint32
	NTPDrops           uint32
	NKEDrops           uint32
	CmdDrops           uint32
	NTPInterval        int8
	NKEInterval        int8
	CmdInterval        int8
	NTPTimeoutInterval int8
	LastNTPHitAgo      uint32
	LastNKEHitAgo      uint32
	LastCmdHitAgo      uint32
}

// ClientAccessesByIndex contains parsed 'clients' reply: chronyd's known
// index count, the next index to resume pagination from, and up to
// MaxClientAccessesByIndex per-client entries.
//
// Pagination is not atomic: chronyd's internal client table can change
// between consecutive page requests, so clients may be missed or seen
// twice when traversing the full table on a busy server.
type ClientAccessesByIndex struct {
	NIndices  uint32
	NextIndex uint32
	NClients  uint32
	Clients   []ClientAccess
}

// ReplyClientAccessesByIndex is a usable version of the 'clients' reply.
type ReplyClientAccessesByIndex struct {
	ReplyHead
	ClientAccessesByIndex
}

func newClientAccess(r *replyClientAccessesByIndexClient) *ClientAccess {
	return &ClientAccess{
		IPAddr:             &r.IPAddr,
		NTPHits:            r.NTPHits,
		NKEHits:            r.NKEHits,
		CmdHits:            r.CmdHits,
		NTPDrops:           r.NTPDrops,
		NKEDrops:           r.NKEDrops,
		CmdDrops:           r.CmdDrops,
		NTPInterval:        r.NTPInterval,
		NKEInterval:        r.NKEInterval,
		CmdInterval:        r.CmdInterval,
		NTPTimeoutInterval: r.NTPTimeoutInterval,
		LastNTPHitAgo:      r.LastNTPHitAgo,
		LastNKEHitAgo:      r.LastNKEHitAgo,
		LastCmdHitAgo:      r.LastCmdHitAgo,
	}
}

// here go request constructors

// NewSourcesPacket creates new packet to request number of sources (peers)
func NewSourcesPacket() *RequestSources {
	return &RequestSources{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqNSources,
		},
		data: [maxDataLen]uint8{},
	}
}

// NewTrackingPacket creates new packet to request 'tracking' information
func NewTrackingPacket() *RequestTracking {
	return &RequestTracking{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqTracking,
		},
		data: [maxDataLen]uint8{},
	}
}

// NewSourceStatsPacket creates a new packet to request 'sourcestats' information
func NewSourceStatsPacket(sourceID int32) *RequestSourceStats {
	return &RequestSourceStats{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqSourceStats,
		},
		Index: sourceID,
		data:  [maxDataLen - 4]uint8{},
	}
}

// NewSourceDataPacket creates new packet to request 'source data' information about source with given ID
func NewSourceDataPacket(sourceID int32) *RequestSourceData {
	return &RequestSourceData{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqSourceData,
		},
		Index: sourceID,
		data:  [maxDataLen - 4]uint8{},
	}
}

// NewNTPDataPacket creates new packet to request 'ntp data' information for given peer IP
func NewNTPDataPacket(ip *IPAddr) *RequestNTPData {
	return &RequestNTPData{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqNTPData,
		},
		IPAddr: *ip,
		data:   [maxDataLen - 16]uint8{},
	}
}

// NewNTPSourceNamePacket creates new packet to request 'source name' information for given peer IP
func NewNTPSourceNamePacket(ip *IPAddr) *RequestNTPSourceName {
	return &RequestNTPSourceName{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqNTPSourceName,
		},
		IPAddr: *ip,
		data:   [maxDataLen - 16]uint8{},
	}
}

// NewAuthDataPacket creates new packet to request 'authdata' information for given peer IP.
// By default chronyd answers it only over the unix socket (ChronySocketPath), otherwise UNAUTH.
func NewAuthDataPacket(ip *IPAddr) *RequestAuthData {
	return &RequestAuthData{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqAuthData,
		},
		IPAddr: *ip,
		data:   [maxDataLen - 16]uint8{},
	}
}

// NewServerStatsPacket creates new packet to request 'serverstats' information
func NewServerStatsPacket() *RequestServerStats {
	return &RequestServerStats{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqServerStats,
		},
		data: [maxDataLen]uint8{},
	}
}

// NewActivityPacket creates new packet to request 'activity' information
func NewActivityPacket() *RequestActivity {
	return &RequestActivity{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqActivity,
		},
		data: [maxDataLen]uint8{},
	}
}

// NewSelectDataPacket creates new packet to request 'selectdata' information
func NewSelectDataPacket(sourceID int32) *RequestSelectData {
	return &RequestSelectData{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqSelectData,
		},
		Index: sourceID,
		data:  [maxDataLen - 4]uint8{},
	}
}

// NewClientAccessesByIndexPacket creates new packet to request 'clients'
// information: a page of up to MaxClientAccessesByIndex per-client entries
// starting at firstIndex. minHits filters out clients with fewer hits.
//
// This sends REQ_CLIENT_ACCESSES_BY_INDEX3 and expects an
// RPY_CLIENT_ACCESSES_BY_INDEX3 reply. chronyd versions older than 4.0 do
// not understand this command and reply with BADPKTVERSION.
//
// chronyd restricts this command to the Unix socket by default (see
// ChronySocketPath); access over UDP port 323 requires an explicit
// 'cmdallow' rule in chrony.conf, otherwise the reply is ACCESSDENIED.
//
// Use NewClientAccessesByIndexResetPacket if you also want chronyd to
// clear its accounting table after the reply.
func NewClientAccessesByIndexPacket(firstIndex, nClients, minHits uint32) *RequestClientAccessesByIndex {
	return newClientAccessesByIndexPacket(firstIndex, nClients, minHits, 0)
}

// NewClientAccessesByIndexResetPacket is like NewClientAccessesByIndexPacket
// but additionally instructs chronyd to clear its accounting table after
// the reply (equivalent to chronyc's `-r` flag). Most polling consumers
// want the non-resetting form, since clearing the table invalidates rate
// calculations across scrape intervals.
func NewClientAccessesByIndexResetPacket(firstIndex, nClients, minHits uint32) *RequestClientAccessesByIndex {
	return newClientAccessesByIndexPacket(firstIndex, nClients, minHits, 1)
}

func newClientAccessesByIndexPacket(firstIndex, nClients, minHits, reset uint32) *RequestClientAccessesByIndex {
	return &RequestClientAccessesByIndex{
		RequestHead: RequestHead{
			Version: protoVersionNumber,
			PKTType: pktTypeCmdRequest,
			Command: reqClientAccessesByIndex3,
		},
		FirstIndex: firstIndex,
		NClients:   nClients,
		MinHits:    minHits,
		Reset:      reset,
		data:       [480]uint8{},
	}
}

// possible clock sources
const (
	ClockSourceUnspec     = "unspec"
	ClockSourcePPS        = "pps"
	ClockSourceLFRadio    = "lf_radio"
	ClockSourceHFRadio    = "hf_radio"
	ClockSourceUHFRadio   = "uhf_radio"
	ClockSourceLocal      = "local"
	ClockSourceNTP        = "ntp"
	ClockSourceOther      = "other"
	ClockSourceWristWatch = "wristwatch"
	ClockSourceTelephone  = "telephone"
)

// ClockSourceDesc stores human-readable descriptions of ClockSource field
var ClockSourceDesc = [10]string{
	ClockSourceUnspec,     // 00
	ClockSourcePPS,        // 01
	ClockSourceLFRadio,    // 02
	ClockSourceHFRadio,    // 03
	ClockSourceUHFRadio,   // 04
	ClockSourceLocal,      // 05
	ClockSourceNTP,        // 06
	ClockSourceOther,      // 07
	ClockSourceWristWatch, // 08
	ClockSourceTelephone,  // 09
}

// decodePacket decodes bytes to valid response packet.
// an easy way to test this is to use 'testchrony' tool we have.
func decodePacket(response []byte) (ResponsePacket, error) {
	var err error
	r := bytes.NewReader(response)
	head := new(ReplyHead)
	if err = binary.Read(r, binary.BigEndian, head); err != nil {
		return nil, err
	}
	Logger.Printf("response head: %+v", head)
	if head.Status != sttSuccess {
		return nil, fmt.Errorf("got status %s (%d)", head.Status, head.Status)
	}
	switch head.Reply {
	case RpyNSources:
		data := new(replySourcesContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplySources{
			ReplyHead: *head,
			NSources:  int(data.NSources),
		}, nil
	case RpySourceData:
		data := new(replySourceDataContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplySourceData{
			ReplyHead:  *head,
			SourceData: *newSourceData(data),
		}, nil
	case RpyTracking:
		data := new(replyTrackingContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyTracking{
			ReplyHead: *head,
			Tracking:  *newTracking(data),
		}, nil
	case RpySourceStats:
		data := new(replySourceStatsContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplySourceStats{
			ReplyHead:   *head,
			SourceStats: *newSourceStats(data),
		}, nil
	case RpyActivity:
		data := new(Activity)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyActivity{
			ReplyHead: *head,
			Activity:  *data,
		}, nil
	case RpyServerStats:
		data := new(ServerStats)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyServerStats{
			ReplyHead:   *head,
			ServerStats: *data,
		}, nil
	case RpyNTPData:
		data := new(replyNTPDataContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyNTPData{
			ReplyHead: *head,
			NTPData:   *newNTPData(data),
		}, nil
	case RpyNTPData2:
		data := new(replyNTPData2Content)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyNTPData2{
			ReplyHead: *head,
			NTPData2:  *newNTPData2(data),
		}, nil
	case RpyNTPSourceName:
		data := new(replyNTPSourceNameContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyNTPSourceName{
			ReplyHead:     *head,
			NTPSourceName: *newNTPSourceName(data),
		}, nil
	case RpyAuthData:
		data := new(replyAuthDataContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyAuthData{
			ReplyHead: *head,
			AuthData:  *newAuthData(data),
		}, nil
	case RpyServerStats2:
		data := new(ServerStats2)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyServerStats2{
			ReplyHead:    *head,
			ServerStats2: *data,
		}, nil
	case RpyServerStats3:
		data := new(ServerStats3)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyServerStats3{
			ReplyHead:    *head,
			ServerStats3: *data,
		}, nil
	case RpyServerStats4:
		data := new(ServerStats4)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplyServerStats4{
			ReplyHead:    *head,
			ServerStats4: *data,
		}, nil
	case RpySelectData:
		data := new(replySelectData)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		return &ReplySelectData{
			ReplyHead:  *head,
			SelectData: *newSelectData(data),
		}, nil
	case RpyClientAccessesByIndex3:
		data := new(replyClientAccessesByIndexContent)
		if err = binary.Read(r, binary.BigEndian, data); err != nil {
			return nil, err
		}
		Logger.Printf("response data: %+v", data)
		n := min(data.NClients, MaxClientAccessesByIndex)
		clients := make([]ClientAccess, n)
		for i := range n {
			clients[i] = *newClientAccess(&data.Clients[i])
		}
		return &ReplyClientAccessesByIndex{
			ReplyHead: *head,
			ClientAccessesByIndex: ClientAccessesByIndex{
				NIndices:  data.NIndices,
				NextIndex: data.NextIndex,
				NClients:  data.NClients,
				Clients:   clients,
			},
		}, nil
	default:
		return nil, fmt.Errorf("not implemented reply type %d from %+v", head.Reply, head)
	}
}
