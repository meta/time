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

package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/facebook/time/calnex/api"
	"github.com/go-ini/ini"
	log "github.com/sirupsen/logrus"
)

// ErrNoMeasureTargets is returned instead of pushing a config that has nothing to
// measure, because measureConfig marks every channel it is not given as unused.
var ErrNoMeasureTargets = errors.New("no measurement targets")

// Calnexes is a map of devices to CalnexConfig
type Calnexes map[string]*CalnexConfig

// CalnexConfig is a config representation of calnex
type CalnexConfig struct {
	Measure        map[api.Channel]MeasureConfig `json:"measure"`
	AntennaDelayNS int                           `json:"antennaDelayNS"`
}

// MeasureConfig is a Calnex channel config
type MeasureConfig struct {
	Target string    `json:"target"`
	Probe  api.Probe `json:"probe"`
	Name   string    `json:"name"`
}

type config struct {
	changed bool
}

// chGet return a value from a section on channel provided
func (c *config) chGet(s *ini.Section, ch api.Channel, key string) string {
	name := fmt.Sprintf("%s\\%s", ch.CalnexAPI(), key)
	if s.HasKey(name) {
		return s.Key(name).Value()
	}
	return ""
}

// chSet modifies a config on several channels
func (c *config) chSet(target string, s *ini.Section, start, end api.Channel, keyf, value string) {
	for i := start.Calnex(); i <= end.Calnex(); i++ {
		ch, _ := api.ChannelFromInt(i)
		name := fmt.Sprintf(keyf, ch.CalnexAPI())
		c.set(target, s, name, value)
	}
}

// set modifies a single config value
func (c *config) set(target string, s *ini.Section, name, value string) {
	if !s.HasKey(name) {
		// if the settings file does not have a setting the unit does not support it
		// and we should not set it
		log.Debugf("%s: key %s does not exist, skipping [there is likely a license missing causing this]", target, name)
		return
	}
	k := s.Key(name)

	if k.Value() != value {
		k.SetValue(value)
		log.Debugf("%s: setting %s to %s", target, name, value)
		c.changed = true
	}
}

func (c *config) measureConfig(target string, s *ini.Section, mc map[api.Channel]MeasureConfig) {
	channelEnabled := make(map[api.Channel]bool)

	for ch, m := range mc {
		// Check that physical channel (not virtual) is installed before trying to change any settings
		if c.chGet(s, ch, "installed") != "1" {
			log.Debugf("%s: no change to channel %s it is not installed", target, ch)
			continue
		}
		channelEnabled[ch] = true
		probe := ""

		switch m.Probe {
		case api.ProbeNTP:
			probe = fmt.Sprintf("%s\\ptp_synce\\mode\\probe_type", ch.CalnexAPI())

			c.set(target, s, fmt.Sprintf("%s\\protocol_enabled", ch.CalnexAPI()), api.ON)
			// Set Virtual Port to use Physical channel 1
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\physical_packet_channel", ch.CalnexAPI()), api.CHANNEL1)

			// Set target we measure
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ntp\\server_ip_ipv6", ch.CalnexAPI()), m.Target)

			// show raw metrics
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ntp\\normalize_delays", ch.CalnexAPI()), api.OFF)
			// use ipv6
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ntp\\protocol_level", ch.CalnexAPI()), api.IPV6)
			// ntp 1 packet per 16 seconds
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ntp\\poll_log_interval", ch.CalnexAPI()), api.INTERVAL)
		case api.ProbePTP:
			probe = fmt.Sprintf("%s\\ptp_synce\\mode\\probe_type", ch.CalnexAPI())

			c.set(target, s, fmt.Sprintf("%s\\protocol_enabled", ch.CalnexAPI()), api.ON)
			// Set Virtual Port to use Physical channel 1
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\physical_packet_channel", ch.CalnexAPI()), api.CHANNEL1)

			// Use SPTP instead of PTPv2
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\version", ch.CalnexAPI()), api.SPTP)

			// Set target we measure
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\master_ip_ipv6", ch.CalnexAPI()), m.Target)

			// use ipv6
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\protocol_level", ch.CalnexAPI()), api.IPV6)

			// ptp 1 packet per 16 seconds
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\log_announce_int", ch.CalnexAPI()), api.INTERVAL)
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\log_delay_req_int", ch.CalnexAPI()), api.INTERVAL)
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\log_sync_int", ch.CalnexAPI()), api.INTERVAL)

			// ptp unicast mode
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\stack_mode", ch.CalnexAPI()), "Unicast")

			// ptp domain
			c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\ptp\\domain", ch.CalnexAPI()), "0")
		case api.ProbePPS:
			probe = fmt.Sprintf("%s\\signal_type", ch.CalnexAPI())

			c.set(target, s, fmt.Sprintf("%s\\server_ip", ch.CalnexAPI()), m.Target)
			c.set(target, s, fmt.Sprintf("%s\\trig_level", ch.CalnexAPI()), "500 mV")
			c.set(target, s, fmt.Sprintf("%s\\freq", ch.CalnexAPI()), "1 Hz")
			c.set(target, s, fmt.Sprintf("%s\\suppress_steps", ch.CalnexAPI()), api.YES)
		}

		// enable NTP/PTP/PPS channels
		c.set(target, s, fmt.Sprintf("%s\\used", ch.CalnexAPI()), api.YES)
		c.set(target, s, probe, m.Probe.CalnexName())
		c.set(target, s, fmt.Sprintf("%s\\channel_name", ch.CalnexAPI()), m.Name)
	}

	// Disable unused channels
	for ch, datatype := range api.MeasureChannelDatatypeMap {
		if !channelEnabled[ch] && c.chGet(s, ch, "installed") == "1" {
			c.set(target, s, fmt.Sprintf("%s\\used", ch.CalnexAPI()), api.NO)
			if datatype == api.TWOWAYTE {
				c.set(target, s, fmt.Sprintf("%s\\protocol_enabled", ch.CalnexAPI()), api.OFF)
				c.set(target, s, fmt.Sprintf("%s\\ptp_synce\\mode\\probe_type", ch.CalnexAPI()), api.DISABLED)
			}
		}
	}
}

func (c *config) baseConfig(target string, measure *ini.Section, gnss *ini.Section, antennaDelayNS int) {
	// device hostname
	c.set(target, measure, "device_name", target)

	// gnss antenna compensation
	if antennaDelayNS < 1000 {
		c.set(target, gnss, "antenna_delay", fmt.Sprintf("%d ns", antennaDelayNS))
	} else {
		c.set(target, gnss, "antenna_delay", fmt.Sprintf("%s us", strconv.FormatFloat(float64(antennaDelayNS)/1000, 'f', -1, 64)))
	}

	// continuous measurement
	c.set(target, measure, "continuous", api.ON)

	// continuous measurement
	c.set(target, measure, "reference", api.INTERNAL)

	// 25h measurement
	c.set(target, measure, "meas_time", "1 days 1 hours")

	// tie_mode=TIE + 1 PPS Alignment
	c.set(target, measure, "tie_mode", "TIE + 1 PPS Alignment")

	// ch8 is a ref channel. Always ON
	c.set(target, measure, "ch8\\used", api.YES)

	maxChannel := api.ChannelONE
	if c.chGet(measure, api.ChannelTWO, "installed") == "1" {
		maxChannel = api.ChannelTWO
	}
	// disable synce
	c.chSet(target, measure, api.ChannelONE, maxChannel, "%s\\synce_enabled", api.OFF)

	// ptp dscp
	c.chSet(target, measure, api.ChannelONE, maxChannel, "%s\\ptp_synce\\ptp\\dscp", "0")

	// DHCP
	c.chSet(target, measure, api.ChannelONE, maxChannel, "%s\\ptp_synce\\ethernet\\dhcp_v6", api.DHCP)
	c.chSet(target, measure, api.ChannelONE, maxChannel, "%s\\ptp_synce\\ethernet\\dhcp_v4", api.DISABLED)

	// enable QSFP FEC for 100G links (first channel only)
	c.chSet(target, measure, api.ChannelONE, api.ChannelONE, "%s\\ptp_synce\\ethernet\\qsfp_fec", api.RSFEC)

	// Enable 1st Physical channel
	c.set(target, measure, fmt.Sprintf("%s\\used", api.ChannelONE.CalnexAPI()), api.YES)
	// Disable 2nd Physical channel only if it is installed
	if maxChannel == api.ChannelTWO {
		c.set(target, measure, fmt.Sprintf("%s\\used", api.ChannelTWO.CalnexAPI()), api.NO)
	}

	// Disable packet measurement on the two physical channel measurements
	c.chSet(target, measure, api.ChannelONE, maxChannel, "%s\\protocol_enabled", api.OFF)

	// Enable virtual channel measurements for channel 1
	c.set(target, measure, fmt.Sprintf("%s\\virtual_channels_enabled", api.ChannelONE.CalnexAPI()), api.ON)
}

// Config configures target Calnex with Network/Calnex configs if apply is specified
func Config(target string, insecureTLS bool, cc *CalnexConfig, apply bool) error {
	if len(cc.Measure) == 0 {
		return ErrNoMeasureTargets
	}

	var c config
	api, err := api.NewAPI(target, insecureTLS, 4*time.Minute)
	if err != nil {
		return err
	}

	f, err := prepare(&c, api, target, cc)
	if err != nil {
		return err
	}

	if !apply {
		log.Infof("%s: dry run. Not pushing config", target)
		return nil
	}

	// check measurement status
	status, err := api.FetchStatus()
	if err != nil {
		return err
	}

	if c.changed {
		if status.MeasurementActive {
			log.Debugf("%s: stopping measurement", target)
			// stop measurement
			if err = api.StopMeasure(); err != nil {
				return err
			}
		}

		log.Infof("%s: pushing the config", target)
		// set the modified config
		if err = api.PushSettings(f); err != nil {
			return err
		}
	} else {
		log.Debugf("%s: no change needs to be applied", target)
	}

	if c.changed || !status.MeasurementActive {
		log.Debugf("%s: starting measurement", target)
		// start measurement
		if err = api.StartMeasure(); err != nil {
			return err
		}
	}

	if err = enableSFTP(api, target, f.Section("network")); err != nil {
		log.Errorf("%s: failed to enable SFTP: %s", target, err)
	}

	return nil
}

// enableSFTP turns on SFTP access, which is how problem reports are fetched.
// sftp_enable only reports the state: pushing it via settings does nothing.
func enableSFTP(calnexAPI *api.API, target string, network *ini.Section) error {
	if network.Key("sftp_enable").Value() != api.OFF {
		return nil
	}
	log.Infof("%s: enabling SFTP", target)
	return calnexAPI.EnableSFTP()
}

// Save saves the Network/Calnex configs to file
func Save(target string, insecureTLS bool, cc *CalnexConfig, saveConfig string) error {
	var c config
	calnexAPI, err := api.NewAPI(target, insecureTLS, time.Minute)
	if err != nil {
		return err
	}

	f, err := prepare(&c, calnexAPI, target, cc)
	if err != nil {
		return err
	}

	buf, err := api.ToBuffer(f)
	if err != nil {
		return err
	}

	if err = os.WriteFile(saveConfig, buf.Bytes(), 0644); err != nil {
		return err
	}

	return nil
}

func prepare(c *config, api *api.API, target string, cc *CalnexConfig) (*ini.File, error) {
	f, err := api.FetchSettings()
	if err != nil {
		return nil, err
	}

	m := f.Section("measure")
	g := f.Section("gnss")

	// set base config
	c.baseConfig(target, m, g, cc.AntennaDelayNS)

	// set measure config
	c.measureConfig(target, m, cc.Measure)

	return f, nil
}

func (cmc *MeasureConfig) isEqual(m *MeasureConfig) bool {
	return cmc.Name == m.Name && cmc.Target == m.Target && cmc.Probe == m.Probe
}

func (cc *CalnexConfig) isEqual(c *CalnexConfig) bool {
	if cc.AntennaDelayNS != c.AntennaDelayNS {
		return false
	}
	if len(cc.Measure) != len(c.Measure) {
		return false
	}
	for ch, m := range cc.Measure {
		cm, ok := c.Measure[ch]
		if !ok {
			return false
		}
		if !m.isEqual(&cm) {
			return false
		}
	}
	return true
}

// IsEqual compares two sets of Calnex configs
func (cs *Calnexes) IsEqual(c *Calnexes) bool {
	if len(*cs) != len(*c) {
		return false
	}
	for target, cc := range *cs {
		cmc, ok := (*c)[target]
		if !ok {
			return false
		}
		if !cc.isEqual(cmc) {
			return false
		}
	}
	return true
}
