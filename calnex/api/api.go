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

package api

import (
	"bytes"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-ini/ini"
)

// API is struct for accessing calnex API
type API struct {
	Client *http.Client
	source string
}

// Status is a struct representing Calnex status JSON response
type Status struct {
	ChannelLinksReady bool
	IPAddressReady    bool
	MeasurementActive bool
	MeasurementReady  bool
	ModulesReady      bool
	ReferenceReady    bool
	IsRecoveryMode    bool
}

// InstrumentStatus is a struct representing Calnex instrument status JSON response
type InstrumentStatus struct {
	Channels       map[Channel]ChannelStatus
	Modules        map[Channel]ModuleStatus
	IsRecoveryMode bool `json:"isRecoveryMode"`
}

// TransceiverStatus is a struct representing Calnex transceiver status
type TransceiverStatus struct {
	Detected bool `json:"detected"`
}

// ChannelStatus is a struct representing Calnex channel instrument status
type ChannelStatus struct {
	Progress int
	QSFP     TransceiverStatus
	Slot     string
	State    string
	Type     string
}

// ModuleStatus is a struct representing Calnex channel module status
type ModuleStatus struct {
	Channels []string
	Progress int
	State    string
	Type     string
}

// Result is a struct representing Calnex result JSON response
type Result struct {
	Result  bool
	Message string
}

// Version is a struct representing Calnex version JSON response
type Version struct {
	Firmware string
}

// Uptime is a struct representing Calnex uptime JSON response
type Uptime struct {
	Uptime int64
}

// SystemMonitor is a struct representing Calnex system monitor JSON response
type SystemMonitor struct {
	CPUUsage        float64 `json:"cpu_usage"`
	FreeMemory      int     `json:"free_memory"`
	InstalledMemory int     `json:"installed_memory"`
	MemoryUsage     int     `json:"memory_usage"`
	Threads         int
}

// GNSS is a struct representing Calnex GNSS JSON response
type GNSS struct {
	AntennaStatus         string
	Locked                bool
	LockedSatellites      int
	SurveyComplete        bool
	SurveyPercentComplete int
}

// PowerSupply is a struct representing a single power supply unit
type PowerSupply struct {
	CommsGood  bool `json:"comms_good"`
	Name       string
	StatusGood bool `json:"status_good"`
}

// PowerSupplyStatus is a struct representing Calnex Power Supply JSON response
type PowerSupplyStatus struct {
	PowerSupplyGood bool `json:"power_supply_good"`
	Supplies        []PowerSupply
}

// RBStatus is a struct representing Calnex Rubidium clock status JSON response
type RBStatus struct {
	RBState     int    `json:"state"`
	RBStateName string `json:"state_name"`
}

// Measurements is the measurement data of every active channel, in device order
type Measurements struct {
	Channels []ChannelMeasurement
}

// ChannelMeasurement is the measurement data of a single channel. The device's
// target and source are not decoded: its targets are incomplete, use the config's.
type ChannelMeasurement struct {
	Channel Channel
	Probe   Probe
	Samples []Sample
}

// Sample is a single measurement point
type Sample struct {
	Timestamp float64 `json:"timestamp"`
	Value     float64 `json:"value"`
}

func (s *Sample) UnmarshalJSON(b []byte) error {
	var payload struct {
		Timestamp *float64 `json:"timestamp"`
		Value     *float64 `json:"value"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return err
	}
	if payload.Timestamp == nil {
		return errors.New("measurement sample has no timestamp")
	}
	if payload.Value == nil {
		return errors.New("measurement sample has no value")
	}
	s.Timestamp = *payload.Timestamp
	s.Value = *payload.Value
	return nil
}

// measurementsResponse is the JSON response.
type measurementsResponse struct {
	Data *Measurements `json:"data"`
}

type channelPayload struct {
	Data *[]Sample `json:"data"`
	Info struct {
		Protocol string `json:"protocol"`
	} `json:"info"`
}

func (m *Measurements) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}

	list, ok := raw["keys"]
	if !ok {
		return errors.New("measurement response has no channel list")
	}

	var order []string
	if err := json.Unmarshal(list, &order); err != nil {
		return fmt.Errorf("failed to parse channel list: %w", err)
	}
	if order == nil {
		return errors.New("measurement response has a null channel list")
	}

	m.Channels = make([]ChannelMeasurement, 0, len(order))
	seenChannels := make(map[Channel]string, len(order))
	for _, name := range order {
		channel, err := ChannelFromString(name)
		if err != nil {
			return fmt.Errorf("unknown channel %s: %w", name, err)
		}
		if _, ok := seenChannels[*channel]; ok {
			return fmt.Errorf("channel %s appears multiple times in the channel list", *channel)
		}
		seenChannels[*channel] = name

		entry, ok := raw[name]
		if !ok {
			return fmt.Errorf("channel %s is in the channel list with no entry", name)
		}

		var payload *channelPayload
		if err := json.Unmarshal(entry, &payload); err != nil {
			return fmt.Errorf("failed to parse channel %s: %w", name, err)
		}

		if payload == nil || payload.Data == nil {
			return fmt.Errorf("channel %s has no data", name)
		}

		probe, err := ProbeFromString(payload.Info.Protocol)
		if err != nil {
			return fmt.Errorf("channel %s has unknown protocol %q: %w", name, payload.Info.Protocol, err)
		}
		m.Channels = append(m.Channels, ChannelMeasurement{
			Channel: *channel,
			Probe:   *probe,
			Samples: *payload.Data,
		})
	}

	for name := range raw {
		if name == "keys" {
			continue
		}
		channel, err := ChannelFromString(name)
		if err != nil || seenChannels[*channel] != name {
			return fmt.Errorf("channel %s has an entry but is not in the channel list", name)
		}
	}
	return nil
}

// Channel is a Calnex channel object
type Channel string

// Channels is a list of Channel
type Channels []Channel

// Calnex Status constants
const (
	ON  = "On"
	OFF = "Off"
	YES = "Yes"
	NO  = "No"

	DISABLED = "Disabled"

	DHCP     = "DHCP"
	TE       = "te"
	TWOWAYTE = "2wayte"
	RSFEC    = "RS-FEC"
	INTERVAL = "1 packet/16 s"
	CHANNEL1 = "Channel 1"
	IPV6     = "UDP/IPv6"
	INTERNAL = "Internal"
	SPTP     = "SPTP_V2.1"
)

// Available Calnex channels
const (
	ChannelA    Channel = "A"
	ChannelB    Channel = "B"
	ChannelC    Channel = "C"
	ChannelD    Channel = "D"
	ChannelE    Channel = "E"
	ChannelF    Channel = "F"
	ChannelONE  Channel = "1"
	ChannelTWO  Channel = "2"
	ChannelREF  Channel = "REF"
	ChannelVP1  Channel = "VP1"
	ChannelVP2  Channel = "VP2"
	ChannelVP3  Channel = "VP3"
	ChannelVP4  Channel = "VP4"
	ChannelVP5  Channel = "VP5"
	ChannelVP6  Channel = "VP6"
	ChannelVP7  Channel = "VP7"
	ChannelVP8  Channel = "VP8"
	ChannelVP9  Channel = "VP9"
	ChannelVP10 Channel = "VP10"
	ChannelVP11 Channel = "VP11"
	ChannelVP12 Channel = "VP12"
	ChannelVP13 Channel = "VP13"
	ChannelVP14 Channel = "VP14"
	ChannelVP15 Channel = "VP15"
	ChannelVP16 Channel = "VP16"
	ChannelVP17 Channel = "VP17"
	ChannelVP18 Channel = "VP18"
	ChannelVP19 Channel = "VP19"
	ChannelVP20 Channel = "VP20"
	ChannelVP21 Channel = "VP21"
	ChannelVP22 Channel = "VP22"
	ChannelVP23 Channel = "VP23"
	ChannelVP24 Channel = "VP24"
	ChannelVP25 Channel = "VP25"
	ChannelVP26 Channel = "VP26"
	ChannelVP27 Channel = "VP27"
	ChannelVP28 Channel = "VP28"
	ChannelVP29 Channel = "VP29"
	ChannelVP30 Channel = "VP30"
	ChannelVP31 Channel = "VP31"
	ChannelVP32 Channel = "VP32"
)

// MeasureChannelDatatypeMap is a Map of the measurement channels to the data type.
// Only channels used for measurements defined here
var MeasureChannelDatatypeMap = map[Channel]string{
	ChannelA:    TE,
	ChannelB:    TE,
	ChannelC:    TE,
	ChannelD:    TE,
	ChannelE:    TE,
	ChannelF:    TE,
	ChannelVP1:  TWOWAYTE,
	ChannelVP2:  TWOWAYTE,
	ChannelVP3:  TWOWAYTE,
	ChannelVP4:  TWOWAYTE,
	ChannelVP5:  TWOWAYTE,
	ChannelVP6:  TWOWAYTE,
	ChannelVP7:  TWOWAYTE,
	ChannelVP8:  TWOWAYTE,
	ChannelVP9:  TWOWAYTE,
	ChannelVP10: TWOWAYTE,
	ChannelVP11: TWOWAYTE,
	ChannelVP12: TWOWAYTE,
	ChannelVP13: TWOWAYTE,
	ChannelVP14: TWOWAYTE,
	ChannelVP15: TWOWAYTE,
	ChannelVP16: TWOWAYTE,
	ChannelVP17: TWOWAYTE,
	ChannelVP18: TWOWAYTE,
	ChannelVP19: TWOWAYTE,
	ChannelVP20: TWOWAYTE,
	ChannelVP21: TWOWAYTE,
	ChannelVP22: TWOWAYTE,
	ChannelVP23: TWOWAYTE,
	ChannelVP24: TWOWAYTE,
	ChannelVP25: TWOWAYTE,
	ChannelVP26: TWOWAYTE,
	ChannelVP27: TWOWAYTE,
	ChannelVP28: TWOWAYTE,
	ChannelVP29: TWOWAYTE,
	ChannelVP30: TWOWAYTE,
	ChannelVP31: TWOWAYTE,
	ChannelVP32: TWOWAYTE,
}

// channelCalnexToInt is a map of Calnex channels to a int variant
var channelCalnexToInt = map[Channel]int{
	ChannelA:    0,
	ChannelB:    1,
	ChannelC:    2,
	ChannelD:    3,
	ChannelE:    4,
	ChannelF:    5,
	ChannelONE:  6,
	ChannelTWO:  7,
	ChannelREF:  8,
	ChannelVP1:  9,
	ChannelVP2:  10,
	ChannelVP3:  11,
	ChannelVP4:  12,
	ChannelVP5:  13,
	ChannelVP6:  14,
	ChannelVP7:  15,
	ChannelVP8:  16,
	ChannelVP9:  17,
	ChannelVP10: 18,
	ChannelVP11: 19,
	ChannelVP12: 20,
	ChannelVP13: 21,
	ChannelVP14: 22,
	ChannelVP15: 23,
	ChannelVP16: 24,
	ChannelVP17: 25,
	ChannelVP18: 26,
	ChannelVP19: 27,
	ChannelVP20: 28,
	ChannelVP21: 29,
	ChannelVP22: 30,
	ChannelVP23: 31,
	ChannelVP24: 32,
	ChannelVP25: 33,
	ChannelVP26: 34,
	ChannelVP27: 35,
	ChannelVP28: 36,
	ChannelVP29: 37,
	ChannelVP30: 38,
	ChannelVP31: 39,
	ChannelVP32: 40,
}

// channelFromInt is a map of String channels to a Calnex variant
var channelFromInt = map[int]Channel{
	0:  ChannelA,
	1:  ChannelB,
	2:  ChannelC,
	3:  ChannelD,
	4:  ChannelE,
	5:  ChannelF,
	6:  ChannelONE,
	7:  ChannelTWO,
	8:  ChannelREF,
	9:  ChannelVP1,
	10: ChannelVP2,
	11: ChannelVP3,
	12: ChannelVP4,
	13: ChannelVP5,
	14: ChannelVP6,
	15: ChannelVP7,
	16: ChannelVP8,
	17: ChannelVP9,
	18: ChannelVP10,
	19: ChannelVP11,
	20: ChannelVP12,
	21: ChannelVP13,
	22: ChannelVP14,
	23: ChannelVP15,
	24: ChannelVP16,
	25: ChannelVP17,
	26: ChannelVP18,
	27: ChannelVP19,
	28: ChannelVP20,
	29: ChannelVP21,
	30: ChannelVP22,
	31: ChannelVP23,
	32: ChannelVP24,
	33: ChannelVP25,
	34: ChannelVP26,
	35: ChannelVP27,
	36: ChannelVP28,
	37: ChannelVP29,
	38: ChannelVP30,
	39: ChannelVP31,
	40: ChannelVP32,
}

// ChannelFromInt returns channel from int
func ChannelFromInt(value int) (*Channel, error) {
	c, ok := channelFromInt[value]
	if !ok {
		return nil, ErrBadChannel
	}
	return &c, nil
}

// ChannelFromString returns channel from string
func ChannelFromString(value string) (*Channel, error) {
	c := Channel(strings.ToUpper(value))
	if _, ok := channelCalnexToInt[c]; !ok {
		return nil, ErrBadChannel
	}
	return &c, nil
}

// UnmarshalText channel from string version
func (c *Channel) UnmarshalText(value []byte) error {
	channel, err := ChannelFromString(string(value))
	if err != nil {
		return err
	}
	*c = *channel
	return nil
}

// Calnex returns calnex friendly channel name like 1 or 7
func (c Channel) Calnex() int {
	return channelCalnexToInt[c]
}

// CalnexAPI returns channel name in API format like "ch2"
func (c Channel) CalnexAPI() string {
	return fmt.Sprintf("ch%d", c.Calnex())
}

// Set Channel to Channels
func (cs *Channels) Set(channel string) error {
	c, err := ChannelFromString(channel)
	if err != nil {
		return err
	}
	*cs = append(*cs, *c)
	return nil
}

// String returns all channels
func (cs *Channels) String() string {
	channels := make([]string, 0, len(*cs))
	for _, c := range *cs {
		channels = append(channels, string(c))
	}
	return strings.Join(channels, ", ")
}

// Type is required by the cobra.Value interface
func (cs *Channels) Type() string {
	return "channel"
}

// Probe is a Calnex probe protocol
type Probe string

// Probe numbers by calnex
const (
	ProbePTP Probe = "PTP"
	ProbeNTP Probe = "NTP"
	ProbePPS Probe = "PPS"
)

// probeCalnexToProbe is a map of Calnex to a probe variant
var probeCalnexAPIToProbe = map[string]Probe{
	"0":     ProbePTP,
	"2":     ProbeNTP,
	"1 PPS": ProbePPS,
}

// probeToCalnexName is a map of probe to a Calnex specific name
var probeToCalnexName = map[Probe]string{
	ProbePTP: "PTP",
	ProbeNTP: "NTP",
	ProbePPS: "1 PPS",
}

// probeToServerType is a map of probe to Calnex server name
var probeToServerType = map[Probe]string{
	ProbePTP: "master_ip_ipv6",
	ProbeNTP: "server_ip_ipv6",
	ProbePPS: "server_ip",
}

// ErrInteralError is returned when Calnex returns 500 Internal server error
var ErrInternalError = errors.New("500 Internal server error")

// ProbeFromString returns Channel object from String version
func ProbeFromString(value string) (*Probe, error) {
	p := Probe(strings.ToUpper(value))
	if _, ok := probeToCalnexName[p]; !ok {
		return nil, errBadProbe
	}
	return &p, nil
}

// ProbeFromCalnex returns Channel object from String version
func ProbeFromCalnex(calnex string) (*Probe, error) {
	p, ok := probeCalnexAPIToProbe[calnex]
	if !ok {
		return nil, errBadProbe
	}
	return &p, nil
}

// UnmarshalText probe from string version
func (p *Probe) UnmarshalText(value []byte) error {
	pr, err := ProbeFromString(string(value))
	if err != nil {
		return err
	}
	*p = *pr
	return nil
}

// CalnexAPI returns probe name (lower case)
func (p Probe) CalnexAPI() string {
	return strings.ToLower(string(p))
}

// ServerType returns server type like "server_ip" or "master_ip"
func (p Probe) ServerType() string {
	return probeToServerType[p]
}

// CalnexName returns Calnex Name like "PTP slave" or "NTP client"
func (p Probe) CalnexName() string {
	return probeToCalnexName[p]
}

const (
	// measureURL is a base URL for to the measurement API
	measureURL = "https://%s/api/get/measure/%s"
	dataURL    = "https://%s/api/getdata?channel=%s&datatype=%s&reset=%t"
	// fetchMeasurementsURL returns every channel in one request
	fetchMeasurementsURL = "https://%s/api/fetch/meta"

	startMeasure = "https://%s/api/startmeasurement"
	stopMeasure  = "https://%s/api/stopmeasurement"

	getSettingsURL      = "https://%s/api/getsettings"
	setSettingsURL      = "https://%s/api/setsettings"
	getStatusURL        = "https://%s/api/getstatus"
	getProblemReportURL = "https://%s/api/getproblemreport"

	clearDeviceURL = "https://%s/api/cleardevice?action=cleardevice"
	rebootURL      = "https://%s/api/reboot?action=reboot"
	resetURL       = "https://%s/api/reset?action=reset"

	versionURL       = "https://%s/api/version"
	uptimeURL        = "https://%s/api/uptime"
	systemMonitorURL = "https://%s/api/monitor/system"
	firmwareURL      = "https://%s/api/updatefirmware"
	certificateURL   = "https://%s/api/installcertificate"
	licenseURL       = "https://%s/api/option/load"

	gnssURL             = "https://%s/api/gnss/status"
	instrumentStatusURL = "https://%s/api/instrument/status"
	powerSupplyURL      = "https://%s/api/getpowersupplyinfo"
	rbURL               = "https://%s/api/rb/status"
	sftpEnableURL       = "https://%s/api/sftp/enable"
)

var (
	// ErrBadChannel is returned when channel is not recognized
	ErrBadChannel = errors.New("channel is not recognized")
	// ErrNotSupported is returned when the device firmware does not implement
	// the endpoint, which it reports as 501 Not Implemented
	ErrNotSupported = errors.New("endpoint not supported by device firmware")
	errBadProbe     = errors.New("probe protocol is not recognized")
	errAPI          = errors.New("invalid response from API")
)

func parseResponse(response string) (string, error) {
	s := strings.Split(strings.TrimSuffix(response, "\n"), "=")
	if len(s) != 2 {
		return "", errAPI
	}

	return s[1], nil
}

// formatHost ensures the device address is properly formatted for URL construction.
// IPv6 addresses are wrapped in brackets per RFC 3986.
// IPv4 and FQDN addresses pass through unchanged.
// Invalid addresses return an error.
func formatHost(source string) (string, error) {
	if source == "" {
		return "", errors.New("device address cannot be empty")
	}

	// Handle already-bracketed input
	if strings.HasPrefix(source, "[") && strings.HasSuffix(source, "]") {
		inner := source[1 : len(source)-1]
		ip := net.ParseIP(inner)
		if ip == nil {
			return "", fmt.Errorf("invalid IP address in brackets: %q", inner)
		}
		if ip.To4() != nil {
			// IPv4 in brackets is an RFC 3986 violation
			return "", fmt.Errorf("IPv4 address must not be bracketed: %q", source)
		}
		// IPv6 in brackets — already correctly formatted
		return source, nil
	}

	// Try parsing as IP address
	if ip := net.ParseIP(source); ip != nil {
		if ip.To4() == nil {
			// IPv6 — must wrap in brackets for URL use
			return "[" + source + "]", nil
		}
		// IPv4 — use as-is
		return source, nil
	}

	// Not an IP — treat as hostname/FQDN.
	// Build a probe URL and verify the input did not bleed into another URL
	// component (path, query, fragment, userinfo). The Host and Path must
	// match exactly what we expect — anything else means the input contained
	// URL-significant characters (e.g. "/", "?", "#", "@") and could later
	// corrupt the API endpoint when substituted into request templates.
	const probePath = "/api/test"
	testURL := fmt.Sprintf("https://%s%s", source, probePath)
	u, err := url.Parse(testURL)
	if err != nil {
		return "", fmt.Errorf("invalid device address %q: %w", source, err)
	}
	if u.Host != source || u.Path != probePath {
		return "", fmt.Errorf("invalid device address %q", source)
	}

	return source, nil
}

// NewAPI returns an pointer of API struct with default values.
func NewAPI(source string, insecureTLS bool, timeout time.Duration) (*API, error) {
	host, err := formatHost(source)
	if err != nil {
		return nil, err
	}
	return &API{
		Client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},
			},
			Timeout: timeout,
		},
		source: host,
	}, nil
}

// FetchCsv takes channel name (like 1, 2, c, d)
// it returns list of CSV lines which is []string
func (a *API) FetchCsv(channel Channel, allData bool) ([][]string, error) {
	url := fmt.Sprintf(dataURL, a.source, channel, MeasureChannelDatatypeMap[channel], allData)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check for empty response
	r := &Result{}
	if err = json.Unmarshal(b, r); err == nil {
		return nil, errors.New(r.Message)
	}

	var res [][]string
	csvReader := csv.NewReader(bytes.NewReader(b))
	csvReader.Comment = '#'
	for {
		csvLine, err := csvReader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("failed to parse csv for data from channel %s: %w", channel, err)
		}
		res = append(res, csvLine)
	}
	return res, nil
}

// fetches all the channel data using the fetch/meta api
func (a *API) FetchMeasurements() (*Measurements, error) {
	url := fmt.Sprintf(fetchMeasurementsURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotImplemented {
		return nil, ErrNotSupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	m := &measurementsResponse{}
	if err = json.NewDecoder(resp.Body).Decode(m); err != nil {
		return nil, err
	}
	if m.Data == nil {
		return nil, errors.New("measurement response has no data section")
	}

	return m.Data, nil
}

// FetchChannelProbe returns monitored protocol of the channel
func (a *API) FetchChannelProbe(channel Channel) (*Probe, error) {
	pth := path.Join(channel.CalnexAPI(), "ptp_synce", "mode", "probe_type")
	if MeasureChannelDatatypeMap[channel] == TE {
		pth = path.Join(channel.CalnexAPI(), "signal_type")
	}
	url := fmt.Sprintf(measureURL, a.source, pth)

	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	probe, err := parseResponse(string(b))
	if err != nil {
		return nil, err
	}
	p, err := ProbeFromCalnex(probe)
	return p, err //nolint:nilnil
}

// FetchChannelTarget returns the measure target of the server monitored on the channel
func (a *API) FetchChannelTarget(channel Channel, probe Probe) (string, error) {
	pth := path.Join(channel.CalnexAPI(), "ptp_synce", probe.CalnexAPI(), probe.ServerType())
	if MeasureChannelDatatypeMap[channel] == TE {
		pth = path.Join(channel.CalnexAPI(), probe.ServerType())
	}
	url := fmt.Sprintf(measureURL, a.source, pth)
	resp, err := a.Client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(http.StatusText(resp.StatusCode))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return parseResponse(string(b))
}

// FetchUsedChannels returns list of channels in use
func (a *API) FetchUsedChannels() ([]Channel, error) {
	channels := []Channel{}
	f, err := a.FetchSettings()
	if err != nil {
		return channels, err
	}
	for ch := range MeasureChannelDatatypeMap {
		chInstalled := f.Section("measure").Key(fmt.Sprintf("%s\\installed", ch.CalnexAPI())).String()
		if chInstalled != "1" {
			continue
		}

		chStatus := f.Section("measure").Key(fmt.Sprintf("%s\\used", ch.CalnexAPI())).String()
		if chStatus == "Yes" {
			channels = append(channels, ch)
		}
	}
	return channels, err
}

// FetchSettings returns the calnex settings
func (a *API) FetchSettings() (*ini.File, error) {
	url := fmt.Sprintf(getSettingsURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	return ini.Load(resp.Body)
}

// FetchStatus returns the calnex status
func (a *API) FetchStatus() (*Status, error) {
	url := fmt.Sprintf(getStatusURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	s := &Status{}
	if err = json.NewDecoder(resp.Body).Decode(s); err != nil {
		return nil, err
	}

	return s, nil
}

// FetchInstrumentStatus returns the calnex instrument status
func (a *API) FetchInstrumentStatus() (*InstrumentStatus, error) {
	url := fmt.Sprintf(instrumentStatusURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}
	i := &InstrumentStatus{}
	if err = json.NewDecoder(resp.Body).Decode(i); err != nil {
		return nil, err
	}
	return i, nil
}

// FetchProblemReport saves a problem report
func (a *API) FetchProblemReport(dir string) (string, error) {
	url := fmt.Sprintf(getProblemReportURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(http.StatusText(resp.StatusCode))
	}

	// calnex_problem_report_2021-12-07_10-42-26.tar
	reportFileName := path.Join(dir, fmt.Sprintf("calnex_problem_report_%s.tar", time.Now().Format("2006-01-02_15-04-05")))
	reportF, err := os.Create(reportFileName)
	if err != nil {
		return "", err
	}
	defer reportF.Close()

	_, err = io.Copy(reportF, resp.Body)
	if err != nil {
		return "", err
	}

	return reportFileName, nil
}

// FetchVersion returns current Firmware Version
func (a *API) FetchVersion() (*Version, error) {
	url := fmt.Sprintf(versionURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	v := &Version{}
	if err = json.NewDecoder(resp.Body).Decode(v); err != nil {
		return nil, err
	}

	return v, nil
}

// PushVersion uploads a new Firmware Version to the device
func (a *API) PushVersion(path string) (*Result, error) {
	fw, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fw.Close()

	url := fmt.Sprintf(firmwareURL, a.source)
	r, err := a.postFile(url, fw)
	return r, err //nolint:nilnil
}

// PushCert uploads a new Certificate to the device
func (a *API) PushCert(cert []byte) (*Result, error) {
	url := fmt.Sprintf(certificateURL, a.source)
	buf := bytes.NewBuffer(cert)

	r, err := a.post(url, buf)
	return r, err //nolint:nilnil
}

// PushLicense uploads a new license to the device
func (a *API) PushLicense(path string) (*Result, error) {
	license, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer license.Close()

	url := fmt.Sprintf(licenseURL, a.source)
	buf := &bytes.Buffer{}
	_, err = buf.ReadFrom(license)

	if err != nil {
		return nil, err
	}

	r, err := a.post(url, buf)
	return r, err //nolint:nilnil
}

// PushSettings pushes the calnex settings
func (a *API) PushSettings(f *ini.File) error {
	buf, err := ToBuffer(f)
	if err != nil {
		return err
	}
	url := fmt.Sprintf(setSettingsURL, a.source)

	_, err = a.post(url, buf)
	return err
}

func (a *API) postFile(url string, content *os.File) (*Result, error) {
	req, err := http.NewRequest(http.MethodPost, url, content)
	if err != nil {
		return nil, err
	}
	fi, err := content.Stat()
	if err != nil {
		return nil, err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Body = io.NopCloser(content)
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(content), nil }

	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send post request with firmware: %w, %s", err, resp.Status)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusInternalServerError {
		return nil, ErrInternalError
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to upload firmware: %s", resp.Status)
	}

	r := &Result{}
	if err = json.NewDecoder(resp.Body).Decode(r); err != nil {
		s, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}
		return nil, fmt.Errorf("failed to decode response, body: %s, err: %w", string(s), err)
	}

	if !r.Result {
		return nil, errors.New(r.Message)
	}

	return r, nil
}

func (a *API) post(url string, content *bytes.Buffer) (*Result, error) {
	// content must be a bytes.Buffer or anything which supports .Len()
	// Otherwise Content-Length will not be set.
	resp, err := a.Client.Post(url, "application/x-www-form-urlencoded", content)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusInternalServerError {
		return nil, ErrInternalError
	}
	r := &Result{}
	if err = json.NewDecoder(resp.Body).Decode(r); err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return r, errors.New(http.StatusText(resp.StatusCode)) //nolint:nilnil
	}

	if !r.Result {
		return nil, errors.New(r.Message)
	}

	return r, nil
}

func (a *API) get(path string) error {
	url := fmt.Sprintf(path, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusInternalServerError {
		return ErrInternalError
	}

	if resp.StatusCode != http.StatusOK {
		return errors.New(http.StatusText(resp.StatusCode))
	}

	r := &Result{}
	if err = json.NewDecoder(resp.Body).Decode(r); err != nil {
		return err
	}

	if !r.Result {
		return errors.New(r.Message)
	}

	return nil
}

// StartMeasure starts measurement
func (a *API) StartMeasure() error {
	return a.get(startMeasure)
}

// StopMeasure stops measurement
func (a *API) StopMeasure() error {
	return a.get(stopMeasure)
}

// ClearDevice clears device data
func (a *API) ClearDevice() error {
	// check measurement status
	status, err := a.FetchStatus()

	if err == nil && status.MeasurementActive {
		// stop measurement if possible
		_ = a.StopMeasure()
	}
	return a.get(clearDeviceURL)
}

// Reboot the device
func (a *API) Reboot(reset bool) error {
	// reset is not graceful, execute immediately
	if reset {
		return a.get(resetURL)
	}

	// check measurement status
	status, err := a.FetchStatus()

	if err == nil && status.MeasurementActive {
		// stop measurement if possible
		_ = a.StopMeasure()
	}
	return a.get(rebootURL)
}

// GnssStatus returns current GNSS status
func (a *API) GnssStatus() (*GNSS, error) {
	url := fmt.Sprintf(gnssURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	g := &GNSS{}
	if err = json.NewDecoder(resp.Body).Decode(g); err != nil {
		return nil, err
	}

	return g, nil
}

// PowerSupplyStatus returns current PSU status
func (a *API) PowerSupplyStatus() (*PowerSupplyStatus, error) {
	url := fmt.Sprintf(powerSupplyURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	p := &PowerSupplyStatus{}
	if err = json.NewDecoder(resp.Body).Decode(p); err != nil {
		return nil, err
	}

	return p, nil
}

// FetchUptime returns uptime of the device
func (a *API) FetchUptime() (*Uptime, error) {
	url := fmt.Sprintf(uptimeURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	u := &Uptime{}
	if err = json.NewDecoder(resp.Body).Decode(u); err != nil {
		return nil, err
	}

	return u, nil
}

// EnableSFTP enables SFTP access
func (a *API) EnableSFTP() error {
	return a.get(sftpEnableURL)
}

// FetchSystemMonitor returns system monitor data
func (a *API) FetchSystemMonitor() (*SystemMonitor, error) {
	url := fmt.Sprintf(systemMonitorURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	s := &SystemMonitor{}
	if err = json.NewDecoder(resp.Body).Decode(s); err != nil {
		return nil, err
	}

	return s, nil
}

// RBStatus returns current Rubidium clock status
func (a *API) RBStatus() (*RBStatus, error) {
	url := fmt.Sprintf(rbURL, a.source)
	resp, err := a.Client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	rb := &RBStatus{}
	if err = json.NewDecoder(resp.Body).Decode(rb); err != nil {
		return nil, err
	}

	return rb, nil
}
