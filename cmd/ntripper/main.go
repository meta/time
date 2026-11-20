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
	"flag"
	"os"

	"github.com/facebook/time/cmd/ntripper/relay"
)

func main() {
	var cfg relay.Config
	cfg.RegisterFlags(flag.CommandLine)
	flag.Parse()
	os.Exit(relay.Main(cfg))
}
