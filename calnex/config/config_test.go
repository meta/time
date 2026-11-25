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
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/facebook/time/calnex/api"
	"github.com/go-ini/ini"
	"github.com/stretchr/testify/require"
)

func TestChSet(t *testing.T) {
	// attempt to set channels 0-5 to "No"
	// but only channels 0-4 exist in the config
	// so only those will be set
	testConfig := "" +
		"[measure]\n" +
		"ch0\\used=Yes\n" +
		"ch1\\used=Yes\n" +
		"ch2\\used=Yes\n" +
		"ch3\\used=Yes\n" +
		"ch4\\used=Yes\n"

	expectedConfig := "" +
		"[measure]\n" +
		"ch0\\used=No\n" +
		"ch1\\used=No\n" +
		"ch2\\used=No\n" +
		"ch3\\used=No\n" +
		"ch4\\used=No\n"
	c := config{}

	f, err := ini.Load([]byte(testConfig))
	require.NoError(t, err)

	s := f.Section("measure")
	c.chSet("leoleovich.com", s, api.ChannelA, api.ChannelF, "%s\\used", api.NO)
	require.True(t, c.changed)

	buf, err := api.ToBuffer(f)
	require.NoError(t, err)
	require.Equal(t, expectedConfig, buf.String())
}

func TestBaseConfig(t *testing.T) {
	// the ch7 settings here are those that will exist in the settings file if there is no license for anything on channel 2
	testConfig := "" +
		"[gnss]\n" +
		"antenna_delay=42 ns\n" +
		"[measure]\n" +
		"continuous=Off\n" +
		"reference=Auto\n" +
		"meas_time=10 minutes\n" +
		"tie_mode=TIE\n" +
		"ch6\\used=Yes\n" +
		"ch8\\used=No\n" +
		"device_name=leoleovich.com\n" +
		"ch6\\synce_enabled=\n" +
		"ch6\\ptp_synce\\ptp\\dscp=\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=\n" +
		"ch6\\protocol_enabled=\n" +
		"ch6\\virtual_channels_enabled=\n" +
		"ch7\\installed=0\n" +
		"ch7\\channel_name=\n" +
		"ch7\\file_name=channel2\n" +
		"ch7\\freq=31.25 MHz\n" +
		"ch7\\notes=\n" +
		"ch7\\phase0=0 s\n" +
		"ch7\\pulse_width=1 s\n" +
		"ch7\\signal_type=31.25 MHz (Ethernet/SyncE)\n" +
		"ch7\\slope=Pos\n" +
		"ch7\\trig_level=0 V\n" +
		"ch7\\type_id=0\n" +
		"ch7\\used=No\n" +
		"ch7\\vmax=0 V\n" +
		"ch7\\vmin=0 V\n"

	expectedConfig := "" +
		"[gnss]\n" +
		"antenna_delay=42 ns\n" +
		"[measure]\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch6\\used=Yes\n" +
		"ch8\\used=Yes\n" +
		"device_name=leoleovich.com\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=RS-FEC\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch6\\virtual_channels_enabled=On\n" +
		"ch7\\installed=0\n" +
		"ch7\\channel_name=\n" +
		"ch7\\file_name=channel2\n" +
		"ch7\\freq=31.25 MHz\n" +
		"ch7\\notes=\n" +
		"ch7\\phase0=0 s\n" +
		"ch7\\pulse_width=1 s\n" +
		"ch7\\signal_type=31.25 MHz (Ethernet/SyncE)\n" +
		"ch7\\slope=Pos\n" +
		"ch7\\trig_level=0 V\n" +
		"ch7\\type_id=0\n" +
		"ch7\\used=No\n" +
		"ch7\\vmax=0 V\n" +
		"ch7\\vmin=0 V\n"

	c := config{}

	f, err := ini.Load([]byte(testConfig))
	require.NoError(t, err)

	s := f.Section("measure")
	g := f.Section("gnss")

	c.baseConfig("leoleovich.com", s, g, 42)
	require.True(t, c.changed)

	buf, err := api.ToBuffer(f)
	require.NoError(t, err)

	require.Equal(t, expectedConfig, buf.String())
}

func TestBaseConfigCh2(t *testing.T) {
	// the ch6 settings here are those that will exist in the settings file if there is no license for anything on channel 2
	testConfig := "[gnss]\n" +
		"antenna_delay=42 ns\n" +
		"[measure]\n" +
		"continuous=Off\n" +
		"reference=Auto\n" +
		"meas_time=10 minutes\n" +
		"tie_mode=TIE\n" +
		"ch7\\installed=1\n" +
		"ch8\\used=No\n" +
		"device_name=\n" +
		"ch7\\synce_enabled=On\n" +
		"ch7\\ptp_synce\\ptp\\dscp=01\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=DHCP\n" +
		"ch7\\used=No\n" +
		"ch7\\protocol_enabled=On\n" +
		"ch6\\channel_name=\n" +
		"ch6\\file_name=channel1\n" +
		"ch6\\freq=31.25 MHz\n" +
		"ch6\\installed=0\n" +
		"ch6\\notes=\n" +
		"ch6\\phase0=0 s\n" +
		"ch6\\pulse_width=1 s\n" +
		"ch6\\signal_type=31.25 MHz (Ethernet/SyncE)\n" +
		"ch6\\slope=Pos\n" +
		"ch6\\trig_level=0 V\n" +
		"ch6\\type_id=0\n" +
		"ch6\\used=No\n" +
		"ch6\\vmax=0 V\n" +
		"ch6\\vmin=0 V\n"

	// The ch6 used flag gets set to Yes which it shouldn't really but it isn't a problem
	expectedConfig := "[gnss]\n" +
		"antenna_delay=42 ns\n" +
		"[measure]\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch7\\installed=1\n" +
		"ch8\\used=Yes\n" +
		"device_name=leoleovich.com\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\used=No\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch6\\channel_name=\n" +
		"ch6\\file_name=channel1\n" +
		"ch6\\freq=31.25 MHz\n" +
		"ch6\\installed=0\n" +
		"ch6\\notes=\n" +
		"ch6\\phase0=0 s\n" +
		"ch6\\pulse_width=1 s\n" +
		"ch6\\signal_type=31.25 MHz (Ethernet/SyncE)\n" +
		"ch6\\slope=Pos\n" +
		"ch6\\trig_level=0 V\n" +
		"ch6\\type_id=0\n" +
		"ch6\\used=Yes\n" +
		"ch6\\vmax=0 V\n" +
		"ch6\\vmin=0 V\n"

	c := config{}

	f, err := ini.Load([]byte(testConfig))
	require.NoError(t, err)

	s := f.Section("measure")
	g := f.Section("gnss")

	c.baseConfig("leoleovich.com", s, g, 42)
	require.True(t, c.changed)

	buf, err := api.ToBuffer(f)
	require.NoError(t, err)
	require.Equal(t, expectedConfig, buf.String())
}

func TestMeasureConfig(t *testing.T) {
	testConfig := "[measure]\n" +
		"ch0\\used=No\n" +
		"ch0\\installed=1\n" +
		"ch1\\used=Yes\n" +
		"ch1\\installed=1\n" +
		"ch2\\used=No\n" +
		"ch2\\installed=1\n" +
		"ch3\\used=Yes\n" +
		"ch3\\installed=1\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=1\n" +
		"ch5\\used=Yes\n" +
		"ch5\\installed=1\n" +
		"ch6\\used=No\n" +
		"ch6\\installed=1\n" +
		"ch7\\used=Yes\n" +
		"ch7\\installed=1\n" +
		"ch8\\used=No\n" +
		"ch8\\installed=1\n" +
		"ch9\\used=No\n" +
		"ch9\\installed=1\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=1\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=1\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=1\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=1\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=1\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=1\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=1\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=1\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=1\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=1\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=1\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=1\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=1\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=1\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=1\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=1\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=1\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=1\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=1\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=1\n" +
		"ch30\\used=No\n" +
		"ch30\\installed=1\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=1\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=1\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=1\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=1\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=1\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=1\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=1\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=1\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=1\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=1\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch6\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch7\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch9\\protocol_enabled=Off\n" +
		"ch9\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch30\\protocol_enabled=Off\n" +
		"ch30\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch0\\server_ip=10.32.1.168\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch0\\trig_level=1 V\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=No\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=2000::000a\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 2\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=On\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv4\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/1 s\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch30\\ptp_synce\\ptp\\version=PTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=2000::000a\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 2\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv4\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Multicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n"

	expectedConfig := "[measure]\n" +
		"ch0\\used=Yes\n" +
		"ch0\\installed=1\n" +
		"ch1\\used=No\n" +
		"ch1\\installed=1\n" +
		"ch2\\used=No\n" +
		"ch2\\installed=1\n" +
		"ch3\\used=No\n" +
		"ch3\\installed=1\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=1\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"ch6\\used=No\n" +
		"ch6\\installed=1\n" +
		"ch7\\used=Yes\n" +
		"ch7\\installed=1\n" +
		"ch8\\used=No\n" +
		"ch8\\installed=1\n" +
		"ch9\\used=Yes\n" +
		"ch9\\installed=1\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=1\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=1\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=1\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=1\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=1\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=1\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=1\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=1\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=1\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=1\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=1\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=1\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=1\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=1\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=1\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=1\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=1\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=1\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=1\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=1\n" +
		"ch30\\used=Yes\n" +
		"ch30\\installed=1\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=1\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=1\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=1\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=1\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=1\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=1\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=1\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=1\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=1\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=1\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch6\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch7\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch9\\protocol_enabled=On\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch30\\protocol_enabled=On\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch0\\server_ip=fd00:3226:301b::1f\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch0\\trig_level=500 mV\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=Yes\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=fd00:3226:301b::3f\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=Off\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv6\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\version=SPTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=fd00:3016:3109:face:0:1:0\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv6\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Unicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n"

	c := config{}

	f, err := ini.Load([]byte(testConfig))
	require.NoError(t, err)

	s := f.Section("measure")

	cc := &CalnexConfig{
		AntennaDelayNS: 42,
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelA: {
				Target: "fd00:3226:301b::1f",
				Probe:  api.ProbePPS,
			},
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
			api.ChannelVP22: {
				Target: "fd00:3016:3109:face:0:1:0",
				Probe:  api.ProbePTP,
			},
		},
	}

	c.measureConfig("leoleovich.com", s, cc.Measure)
	require.True(t, c.changed)

	buf, err := api.ToBuffer(f)
	require.NoError(t, err)
	require.Equal(t, expectedConfig, buf.String())
}

func TestMeasureConfig2(t *testing.T) {
	// similar to above but has all channels not installed so their settings should not change
	testConfig := "[measure]\n" +
		"ch0\\used=No\n" +
		"ch0\\installed=0\n" +
		"ch1\\used=Yes\n" +
		"ch1\\installed=0\n" +
		"ch2\\used=No\n" +
		"ch2\\installed=0\n" +
		"ch3\\used=Yes\n" +
		"ch3\\installed=0\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=0\n" +
		"ch5\\used=Yes\n" +
		"ch5\\installed=0\n" +
		"ch6\\used=No\n" +
		"ch6\\installed=0\n" +
		"ch7\\used=Yes\n" +
		"ch7\\installed=0\n" +
		"ch8\\used=No\n" +
		"ch8\\installed=0\n" +
		"ch9\\used=No\n" +
		"ch9\\installed=0\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=0\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=0\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=0\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=0\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=0\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=0\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=0\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=0\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=0\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=0\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=0\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=0\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=0\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=0\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=0\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=0\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=0\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=0\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=0\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=0\n" +
		"ch30\\used=No\n" +
		"ch30\\installed=0\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=0\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=0\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=0\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=0\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=0\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=0\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=0\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=0\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=0\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=0\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch6\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch7\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch0\\server_ip=10.32.1.168\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch0\\trig_level=1 V\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=No\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=2000::000a\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 2\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=On\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv4\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/1 s\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch30\\ptp_synce\\ptp\\version=PTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=2000::000a\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 2\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv4\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Multicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n"

	c := config{}

	f, err := ini.Load([]byte(testConfig))
	require.NoError(t, err)

	s := f.Section("measure")

	cc := &CalnexConfig{
		AntennaDelayNS: 42,
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelA: {
				Target: "fd00:3226:301b::1f",
				Probe:  api.ProbePPS,
			},
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
			api.ChannelVP22: {
				Target: "fd00:3016:3109:face:0:1:0",
				Probe:  api.ProbePTP,
			},
		},
	}

	c.measureConfig("leoleovich.com", s, cc.Measure)
	require.False(t, c.changed) //should not have changed in this case

	buf, err := api.ToBuffer(f)
	require.NoError(t, err)
	require.Equal(t, testConfig, buf.String())
}

func TestConfig(t *testing.T) {
	expectedConfig := "[gnss]\n" +
		"antenna_delay=4.2 us\n" +
		"[measure]\n" +
		"device_name=%s\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch0\\used=No\n" +
		"ch0\\installed=1\n" +
		"ch1\\used=No\n" +
		"ch1\\installed=1\n" +
		"ch3\\used=No\n" +
		"ch3\\installed=1\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=1\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"ch6\\used=Yes\n" +
		"ch6\\installed=1\n" +
		"ch7\\used=No\n" +
		"ch7\\installed=1\n" +
		"ch8\\used=Yes\n" +
		"ch8\\installed=1\n" +
		"ch9\\used=Yes\n" +
		"ch9\\installed=1\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=1\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=1\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=1\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=1\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=1\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=1\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=1\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=1\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=1\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=1\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=1\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=1\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=1\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=1\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=1\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=1\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=1\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=1\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=1\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=1\n" +
		"ch30\\used=Yes\n" +
		"ch30\\installed=1\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=1\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=1\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=1\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=1\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=1\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=1\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=1\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=1\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=1\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=1\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch6\\virtual_channels_enabled=On\n" +
		"ch9\\protocol_enabled=On\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch30\\protocol_enabled=On\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=RS-FEC\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=fd00:3226:301b::3f\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=Off\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv6\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\version=SPTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=fd00:3016:3109:face:0:1:0\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv6\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Unicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n" +
		"ch2\\cable_compensation=0 s\n" +
		"ch2\\calibration_date=\n" +
		"ch2\\channel_name=timeserver01.example.com\n" +
		"ch2\\file_name=channelC\n" +
		"ch2\\filter=Off\n" +
		"ch2\\freq=1 Hz\n" +
		"ch2\\impedance=75 Ohm\n" +
		"ch2\\notes=\n" +
		"ch2\\phase0=0 s\n" +
		"ch2\\pps_converter_used=No\n" +
		"ch2\\pulse_width=1 s\n" +
		"ch2\\server_ip=fd00:3226:301b::1f\n" +
		"ch2\\signal_type=1 PPS\n" +
		"ch2\\slope=Pos\n" +
		"ch2\\suppress_steps=Yes\n" +
		"ch2\\trig_level=500 mV\n" +
		"ch2\\type_id=1\n" +
		"ch2\\used=Yes\n" +
		"ch2\\installed=1\n" +
		"ch2\\vmax=0 V\n" +
		"ch2\\vmin=0 V\n"

	testConfig := "" +
		"[measure]\n" +
		"ch0\\used=Yes\n" +
		"ch0\\installed=1\n" +
		"ch6\\used=Yes\n" +
		"ch6\\installed=1\n" +
		"ch9\\used=Yes\n" +
		"ch9\\installed=1\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=1\n" +
		"device_name=%s\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch8\\used=Yes\n" +
		"ch8\\installed=1\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=RS-FEC\n" +
		"ch7\\used=No\n" +
		"ch7\\installed=1\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch6\\virtual_channels_enabled=On\n" +
		"ch9\\protocol_enabled=Off\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=2000::64\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=On\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv4\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/s\n" +
		"ch9\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch30\\protocol_enabled=Off\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch30\\ptp_synce\\ptp\\version=PTP_V2.0\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=2000::4:1\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IP4\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=16 packet/s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=16 packet/s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Multicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=24\n" +
		"ch30\\used=No\n" +
		"ch30\\installed=1\n" +
		"ch30\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=1\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=1\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=1\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=1\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=1\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=1\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=1\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=1\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=1\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=1\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=1\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=1\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=1\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch3\\used=No\n" +
		"ch3\\installed=1\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=1\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=1\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=1\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=1\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=1\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=1\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=1\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=1\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=1\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=1\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=1\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=1\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=1\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=1\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=1\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=1\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch1\\used=No\n" +
		"ch1\\installed=1\n" +
		"ch2\\used=No\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=1\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"ch2\\cable_compensation=0 s\n" +
		"ch2\\calibration_date=\n" +
		"ch2\\channel_name=\n" +
		"ch2\\file_name=channelC\n" +
		"ch2\\filter=Off\n" +
		"ch2\\freq=1 Hz\n" +
		"ch2\\impedance=75 Ohm\n" +
		"ch2\\notes=\n" +
		"ch2\\phase0=0 s\n" +
		"ch2\\pps_converter_used=No\n" +
		"ch2\\pulse_width=1 s\n" +
		"ch2\\server_ip=\n" +
		"ch2\\signal_type=Unknown Signal\n" +
		"ch2\\slope=Pos\n" +
		"ch2\\suppress_steps=No\n" +
		"ch2\\trig_level=0 V\n" +
		"ch2\\type_id=1\n" +
		"ch2\\used=Yes\n" +
		"ch2\\installed=1\n" +
		"ch2\\vmax=0 V\n" +
		"ch2\\vmin=0 V\n" +
		"[gnss]\n" +
		"antenna_delay=42 ns\n"

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if strings.Contains(r.URL.Path, "getsettings") {
			// FetchSettings
			testConfig = fmt.Sprintf(testConfig, r.Host)
			fmt.Fprintln(w, testConfig)
		} else if strings.Contains(r.URL.Path, "getstatus") {
			// FetchStatus
			fmt.Fprintln(w, "{\n\"referenceReady\": true,\n\"modulesReady\": true,\n\"measurementActive\": true\n}")
		} else if strings.Contains(r.URL.Path, "stopmeasurement") {
			// StopMeasure
			fmt.Fprintln(w, "{\n\"result\": true\n}")
		} else if strings.Contains(r.URL.Path, "setsettings") {
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			// Config comes back shuffled every time
			require.ElementsMatch(t, strings.Split(expectedConfig, "\n"), strings.Split(string(b), "\n"))
			// PushSettings
			fmt.Fprintln(w, "{\n\"result\": true\n}")
		} else if strings.Contains(r.URL.Path, "startmeasurement") {
			// StartMeasure
			fmt.Fprintln(w, "{\n\"result\": true\n}")
		}
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := api.NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()
	expectedConfig = fmt.Sprintf(expectedConfig, parsed.Host)

	cc := &CalnexConfig{
		AntennaDelayNS: 4200,
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelC: {
				Target: "fd00:3226:301b::1f",
				Probe:  api.ProbePPS,
				Name:   "timeserver01.example.com",
			},
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
			api.ChannelVP22: {
				Target: "fd00:3016:3109:face:0:1:0",
				Probe:  api.ProbePTP,
			},
		},
	}

	err = Config(parsed.Host, true, cc, true)
	require.NoError(t, err)
}

func TestConfigSFTP(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sftpEnable  string
		enableFails bool
		wantEnables int32
	}{
		{name: "off", sftpEnable: api.OFF, wantEnables: 1},
		{name: "on", sftpEnable: api.ON},
		{name: "enable fails", sftpEnable: api.OFF, enableFails: true, wantEnables: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var enables atomic.Int32
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/getsettings":
					fmt.Fprintf(w, "[network]\nsftp_enable=%s\n", tc.sftpEnable)
				case "/api/getstatus":
					fmt.Fprintln(w, `{"measurementActive": true}`)
				case "/api/sftp/enable":
					enables.Add(1)
					if tc.enableFails {
						http.Error(w, "SFTP unavailable", http.StatusServiceUnavailable)
						return
					}
					fmt.Fprintln(w, `{"result": true}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()

			parsed, err := url.Parse(ts.URL)
			require.NoError(t, err)
			cc := &CalnexConfig{
				Measure: map[api.Channel]MeasureConfig{
					api.ChannelA: {Target: "1 PPS", Probe: api.ProbePPS},
				},
			}

			require.NoError(t, Config(parsed.Host, true, cc, true))
			require.Equal(t, tc.wantEnables, enables.Load())
		})
	}
}

func TestConfigFail(t *testing.T) {
	cc := &CalnexConfig{
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
		},
	}

	err := Config("localhost", true, cc, true)
	require.Error(t, err)
}

func TestConfigNoMeasureTargets(t *testing.T) {
	var reached atomic.Int64
	ts := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		t.Logf("device was contacted: %s", r.URL.Path)
	}))
	defer ts.Close()

	parsed, err := url.Parse(ts.URL)
	require.NoError(t, err)

	err = Config(parsed.Host, true, &CalnexConfig{AntennaDelayNS: 4200}, true)
	require.ErrorIs(t, err, ErrNoMeasureTargets)

	ts.Close()
	require.Zero(t, reached.Load())
}

func TestJSONExport(t *testing.T) {
	expected := `{"measure":{"A":{"target":"1 PPS","probe":"PPS","name":""},"VP1":{"target":"fd00:3226:301b::3f","probe":"NTP","name":""},"VP22":{"target":"fd00:3016:3109:face:0:1:0","probe":"PTP","name":""}},"antennaDelayNS":42}`
	cc := CalnexConfig{
		AntennaDelayNS: 42,
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelA: {
				Target: "1 PPS",
				Probe:  api.ProbePPS,
			},
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
			api.ChannelVP22: {
				Target: "fd00:3016:3109:face:0:1:0",
				Probe:  api.ProbePTP,
			},
		},
	}

	jsonData, err := json.Marshal(cc)
	require.NoError(t, err)
	require.Equal(t, expected, string(jsonData))
}

func TestSaveConfig(t *testing.T) {
	expectedConfig := "" +
		"[measure]\n" +
		"ch0\\used=Yes\n" +
		"ch6\\used=Yes\n" +
		"ch9\\used=Yes\n" +
		"ch22\\used=No\n" +
		"device_name=%s\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch8\\used=Yes\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=RS-FEC\n" +
		"ch7\\used=No\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch6\\virtual_channels_enabled=On\n" +
		"ch0\\server_ip=fd00:3226:301b::1f\n" +
		"ch0\\trig_level=500 mV\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=Yes\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch9\\protocol_enabled=On\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=fd00:3226:301b::3f\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=Off\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv6\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/16 s\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch30\\protocol_enabled=On\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch30\\ptp_synce\\ptp\\version=SPTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=fd00:3016:3109:face:0:1:0\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv6\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Unicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n" +
		"ch30\\used=Yes\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch33\\used=No\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\used=No\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\used=No\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch31\\used=No\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\used=No\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\used=No\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\used=No\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\used=No\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\used=No\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch10\\used=No\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\used=No\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\used=No\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\used=No\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch3\\used=No\n" +
		"ch19\\used=No\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\used=No\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\used=No\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch4\\used=No\n" +
		"ch18\\used=No\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\used=No\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\used=No\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\used=No\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\used=No\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\used=No\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\used=No\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\used=No\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\used=No\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\used=No\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\used=No\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\used=No\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch1\\used=No\n" +
		"ch2\\used=No\n" +
		"ch24\\used=No\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"[gnss]\n" +
		"antenna_delay=42 ns\n"

	testConfig := "" +
		"[measure]\n" +
		"ch0\\used=Yes\n" +
		"ch6\\used=Yes\n" +
		"ch9\\used=Yes\n" +
		"ch22\\used=No\n" +
		"device_name=%s\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch8\\used=Yes\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=RS-FEC\n" +
		"ch7\\used=No\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch6\\virtual_channels_enabled=On\n" +
		"ch0\\server_ip=fd00:3226:301b::1f\n" +
		"ch0\\trig_level=500 mV\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=Yes\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch9\\protocol_enabled=On\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=fd00:3226:301b::3f\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=Off\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv6\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/16 s\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch30\\protocol_enabled=On\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch30\\ptp_synce\\ptp\\version=SPTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=fd00:3016:3109:face:0:1:0\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv6\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Unicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n" +
		"ch30\\used=Yes\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch33\\used=No\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\used=No\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\used=No\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch31\\used=No\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\used=No\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\used=No\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\used=No\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\used=No\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\used=No\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch10\\used=No\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\used=No\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\used=No\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\used=No\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch3\\used=No\n" +
		"ch19\\used=No\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\used=No\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\used=No\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch4\\used=No\n" +
		"ch18\\used=No\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\used=No\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\used=No\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\used=No\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\used=No\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\used=No\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\used=No\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\used=No\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\used=No\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\used=No\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\used=No\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\used=No\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch1\\used=No\n" +
		"ch2\\used=No\n" +
		"ch24\\used=No\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"[gnss]\n" +
		"antenna_delay=42 ns\n"

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		// replacve the host with the test server host
		testConfig = fmt.Sprintf(testConfig, r.Host)
		fmt.Fprintln(w, testConfig)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := api.NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()
	// add rubbish to end of expected config????
	expectedConfig = fmt.Sprintf(expectedConfig, parsed.Host)
	cc := &CalnexConfig{
		AntennaDelayNS: 42,
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelA: {
				Target: "fd00:3226:301b::1f",
				Probe:  api.ProbePPS,
			},
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
			api.ChannelVP22: {
				Target: "fd00:3016:3109:face:0:1:0",
				Probe:  api.ProbePTP,
			},
		},
	}

	// Prepare tmp config file location
	f, err := os.CreateTemp("/tmp", "calnex")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	defer f.Close()

	err = Save(parsed.Host, true, cc, f.Name())
	require.NoError(t, err)

	savedConfig, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	require.ElementsMatch(t, strings.Split(expectedConfig, "\n"), strings.Split(string(savedConfig), "\n"))
}

func TestSaveConfigFail(t *testing.T) {
	cc := &CalnexConfig{Measure: map[api.Channel]MeasureConfig{}}

	// Prepare tmp config file location
	f, err := os.CreateTemp("/tmp", "calnex")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	defer f.Close()

	err = Save("localhost", true, cc, f.Name())
	require.Error(t, err)
}

func TestPrepare(t *testing.T) {
	// start with relatively complete config as it should be set up by previous functions so ensure no unexpected changes to other settings
	testConfig := "[gnss]\n" +
		"antenna_delay=650 ns\n" +
		"[measure]\n" +
		"continuous=Off\n" +
		"reference=Auto\n" +
		"meas_time=10 minutes\n" +
		"tie_mode=TIE\n" +
		"ch6\\used=Yes\n" +
		"ch6\\installed=1\n" +
		"ch8\\used=Yes\n" +
		"ch8\\installed=1\n" +
		"ch0\\used=Yes\n" +
		"ch0\\installed=1\n" +
		"ch1\\used=No\n" +
		"ch1\\installed=1\n" +
		"ch2\\used=No\n" +
		"ch2\\installed=1\n" +
		"ch3\\used=No\n" +
		"ch3\\installed=1\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=1\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"ch7\\used=No\n" +
		"ch7\\installed=1\n" +
		"ch9\\used=Yes\n" +
		"ch9\\installed=1\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=1\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=1\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=1\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=1\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=1\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=1\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=1\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=1\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=1\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=1\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=1\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=1\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=1\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=1\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=1\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=1\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=1\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=1\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=1\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=1\n" +
		"ch30\\used=Yes\n" +
		"ch30\\installed=1\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=1\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=1\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=1\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=1\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=1\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=1\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=1\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=1\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=1\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=1\n" +
		"ch6\\protocol_enabled=Yes\n" +
		"ch6\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch7\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch9\\protocol_enabled=Off\n" +
		"ch9\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\protocol_enabled=On\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\protocol_enabled=On\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch30\\protocol_enabled=On\n" +
		"ch30\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch0\\server_ip=10.32.1.168\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch0\\trig_level=1 V\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=No\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=2000::000a\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 2\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=On\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv4\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/1 s\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch30\\ptp_synce\\ptp\\version=PTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=2000::000a\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 2\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv4\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/1 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Multicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=1\n" +
		"device_name=\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=\n" +
		"ch6\\virtual_channels_enabled=\n"

	expectedConfig := "[gnss]\n" +
		"antenna_delay=1.042 us\n" +
		"[measure]\n" +
		"continuous=On\n" +
		"reference=Internal\n" +
		"meas_time=1 days 1 hours\n" +
		"tie_mode=TIE + 1 PPS Alignment\n" +
		"ch6\\used=Yes\n" +
		"ch6\\installed=1\n" +
		"ch8\\used=Yes\n" +
		"ch8\\installed=1\n" +
		"ch0\\used=Yes\n" +
		"ch0\\installed=1\n" +
		"ch1\\used=No\n" +
		"ch1\\installed=1\n" +
		"ch2\\used=No\n" +
		"ch2\\installed=1\n" +
		"ch3\\used=No\n" +
		"ch3\\installed=1\n" +
		"ch4\\used=No\n" +
		"ch4\\installed=1\n" +
		"ch5\\used=No\n" +
		"ch5\\installed=1\n" +
		"ch7\\used=No\n" +
		"ch7\\installed=1\n" +
		"ch9\\used=Yes\n" +
		"ch9\\installed=1\n" +
		"ch10\\used=No\n" +
		"ch10\\installed=1\n" +
		"ch11\\used=No\n" +
		"ch11\\installed=1\n" +
		"ch12\\used=No\n" +
		"ch12\\installed=1\n" +
		"ch13\\used=No\n" +
		"ch13\\installed=1\n" +
		"ch14\\used=No\n" +
		"ch14\\installed=1\n" +
		"ch15\\used=No\n" +
		"ch15\\installed=1\n" +
		"ch16\\used=No\n" +
		"ch16\\installed=1\n" +
		"ch17\\used=No\n" +
		"ch17\\installed=1\n" +
		"ch18\\used=No\n" +
		"ch18\\installed=1\n" +
		"ch19\\used=No\n" +
		"ch19\\installed=1\n" +
		"ch20\\used=No\n" +
		"ch20\\installed=1\n" +
		"ch21\\used=No\n" +
		"ch21\\installed=1\n" +
		"ch22\\used=No\n" +
		"ch22\\installed=1\n" +
		"ch23\\used=No\n" +
		"ch23\\installed=1\n" +
		"ch24\\used=No\n" +
		"ch24\\installed=1\n" +
		"ch25\\used=No\n" +
		"ch25\\installed=1\n" +
		"ch26\\used=No\n" +
		"ch26\\installed=1\n" +
		"ch27\\used=No\n" +
		"ch27\\installed=1\n" +
		"ch28\\used=No\n" +
		"ch28\\installed=1\n" +
		"ch29\\used=No\n" +
		"ch29\\installed=1\n" +
		"ch30\\used=Yes\n" +
		"ch30\\installed=1\n" +
		"ch31\\used=No\n" +
		"ch31\\installed=1\n" +
		"ch32\\used=No\n" +
		"ch32\\installed=1\n" +
		"ch33\\used=No\n" +
		"ch33\\installed=1\n" +
		"ch34\\used=No\n" +
		"ch34\\installed=1\n" +
		"ch35\\used=No\n" +
		"ch35\\installed=1\n" +
		"ch36\\used=No\n" +
		"ch36\\installed=1\n" +
		"ch37\\used=No\n" +
		"ch37\\installed=1\n" +
		"ch38\\used=No\n" +
		"ch38\\installed=1\n" +
		"ch39\\used=No\n" +
		"ch39\\installed=1\n" +
		"ch40\\used=No\n" +
		"ch40\\installed=1\n" +
		"ch6\\protocol_enabled=Off\n" +
		"ch6\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch7\\protocol_enabled=Off\n" +
		"ch7\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch9\\protocol_enabled=On\n" +
		"ch9\\ptp_synce\\mode\\probe_type=NTP\n" +
		"ch10\\protocol_enabled=Off\n" +
		"ch10\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch11\\protocol_enabled=Off\n" +
		"ch11\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch12\\protocol_enabled=Off\n" +
		"ch12\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch13\\protocol_enabled=Off\n" +
		"ch13\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch14\\protocol_enabled=Off\n" +
		"ch14\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch15\\protocol_enabled=Off\n" +
		"ch15\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch16\\protocol_enabled=Off\n" +
		"ch16\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch17\\protocol_enabled=Off\n" +
		"ch17\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch18\\protocol_enabled=Off\n" +
		"ch18\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch19\\protocol_enabled=Off\n" +
		"ch19\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch20\\protocol_enabled=Off\n" +
		"ch20\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch21\\protocol_enabled=Off\n" +
		"ch21\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch22\\protocol_enabled=Off\n" +
		"ch22\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch23\\protocol_enabled=Off\n" +
		"ch23\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch24\\protocol_enabled=Off\n" +
		"ch24\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch25\\protocol_enabled=Off\n" +
		"ch25\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch26\\protocol_enabled=Off\n" +
		"ch26\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch27\\protocol_enabled=Off\n" +
		"ch27\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch28\\protocol_enabled=Off\n" +
		"ch28\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch29\\protocol_enabled=Off\n" +
		"ch29\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch30\\protocol_enabled=On\n" +
		"ch30\\ptp_synce\\mode\\probe_type=PTP\n" +
		"ch31\\protocol_enabled=Off\n" +
		"ch31\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch32\\protocol_enabled=Off\n" +
		"ch32\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch33\\protocol_enabled=Off\n" +
		"ch33\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch34\\protocol_enabled=Off\n" +
		"ch34\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch35\\protocol_enabled=Off\n" +
		"ch35\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch36\\protocol_enabled=Off\n" +
		"ch36\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch37\\protocol_enabled=Off\n" +
		"ch37\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch38\\protocol_enabled=Off\n" +
		"ch38\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch39\\protocol_enabled=Off\n" +
		"ch39\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch40\\protocol_enabled=Off\n" +
		"ch40\\ptp_synce\\mode\\probe_type=Disabled\n" +
		"ch0\\server_ip=fd00:3226:301b::1f\n" +
		"ch0\\signal_type=1 PPS\n" +
		"ch0\\trig_level=500 mV\n" +
		"ch0\\freq=1 Hz\n" +
		"ch0\\suppress_steps=Yes\n" +
		"ch9\\ptp_synce\\ntp\\server_ip_ipv6=fd00:3226:301b::3f\n" +
		"ch9\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch9\\ptp_synce\\ntp\\normalize_delays=Off\n" +
		"ch9\\ptp_synce\\ntp\\protocol_level=UDP/IPv6\n" +
		"ch9\\ptp_synce\\ntp\\poll_log_interval=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\version=SPTP_V2.1\n" +
		"ch30\\ptp_synce\\ptp\\master_ip_ipv6=fd00:3016:3109:face:0:1:0\n" +
		"ch30\\ptp_synce\\physical_packet_channel=Channel 1\n" +
		"ch30\\ptp_synce\\ptp\\protocol_level=UDP/IPv6\n" +
		"ch30\\ptp_synce\\ptp\\log_announce_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_delay_req_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\log_sync_int=1 packet/16 s\n" +
		"ch30\\ptp_synce\\ptp\\stack_mode=Unicast\n" +
		"ch30\\ptp_synce\\ptp\\domain=0\n" +
		"device_name=leoleovich.com\n" +
		"ch6\\synce_enabled=Off\n" +
		"ch7\\synce_enabled=Off\n" +
		"ch6\\ptp_synce\\ptp\\dscp=0\n" +
		"ch7\\ptp_synce\\ptp\\dscp=0\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v6=DHCP\n" +
		"ch6\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch7\\ptp_synce\\ethernet\\dhcp_v4=Disabled\n" +
		"ch6\\ptp_synce\\ethernet\\qsfp_fec=RS-FEC\n" +
		"ch6\\virtual_channels_enabled=On\n"

	var c config
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		fmt.Fprintln(w, testConfig)
	}))
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	calnexAPI, err := api.NewAPI(parsed.Host, true, time.Second)
	require.NoError(t, err)
	calnexAPI.Client = ts.Client()
	cc := &CalnexConfig{
		AntennaDelayNS: 1042,
		Measure: map[api.Channel]MeasureConfig{
			api.ChannelA: {
				Target: "fd00:3226:301b::1f",
				Probe:  api.ProbePPS,
			},
			api.ChannelVP1: {
				Target: "fd00:3226:301b::3f",
				Probe:  api.ProbeNTP,
			},
			api.ChannelVP22: {
				Target: "fd00:3016:3109:face:0:1:0",
				Probe:  api.ProbePTP,
			},
		},
	}

	f, err := prepare(&c, calnexAPI, "leoleovich.com", cc)
	require.NoError(t, err)
	require.True(t, c.changed)

	buf, err := api.ToBuffer(f)
	require.NoError(t, err)
	require.Equal(t, expectedConfig, buf.String())
}

func TestConfigComparison(t *testing.T) {
	measures := map[api.Channel]MeasureConfig{
		api.ChannelC: {
			Target: "::5",
			Probe:  api.ProbePPS,
			Name:   "name5",
		},
		api.ChannelE: {
			Target: "::7",
			Probe:  api.ProbePPS,
			Name:   "name7",
		},
		api.ChannelF: {
			Target: "::8",
			Probe:  api.ProbePPS,
			Name:   "name8",
		},
		api.ChannelVP1: {
			Target: "::5",
			Probe:  api.ProbeNTP,
			Name:   "name5",
		},
		api.ChannelVP2: {
			Target: "::7",
			Probe:  api.ProbeNTP,
			Name:   "name7",
		},
	}
	cfg1 := &CalnexConfig{
		AntennaDelayNS: 672,
		Measure:        map[api.Channel]MeasureConfig{},
	}
	cfg2 := &CalnexConfig{}
	*cfg2 = *cfg1
	cfg1.Measure = measures
	maps.Copy(cfg2.Measure, measures)
	cs1 := &Calnexes{"calnex01.zzz1.test.com": cfg1}
	cs2 := &Calnexes{"calnex01.zzz1.test.com": cfg2}
	require.True(t, cs1.IsEqual(cs2))
	cfg2.AntennaDelayNS = 670
	require.False(t, cs1.IsEqual(cs2))
	cfg2.AntennaDelayNS = 672
	cfg2.Measure[api.ChannelVP3] = MeasureConfig{
		Target: "::7",
		Probe:  api.ProbePTP,
		Name:   "name7",
	}
	require.False(t, cs1.IsEqual(cs2))
}
