# Time

[![lint](https://github.com/facebook/time/actions/workflows/lint.yml/badge.svg)](https://github.com/facebook/time/actions/workflows/lint.yml)
[![test](https://github.com/facebook/time/actions/workflows/test.yml/badge.svg)](https://github.com/facebook/time/actions/workflows/test.yml)
[![codecov](https://codecov.io/gh/facebook/time/branch/main/graph/badge.svg?token=vucpIU4aIq)](https://codecov.io/gh/facebook/time)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/facebook/time)
[![GoDoc](https://pkg.go.dev/badge/github.com/facebook/time?status.svg)](https://pkg.go.dev/github.com/facebook/time?tab=doc)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

# Contents

- [Documentation](#Documentation)
- [License](#License)

## Documentation

Collection of Meta's Time Libraries such as NTP and PTP

### cmd
All executables provided by this repo.

### NTP
NTP-specific libraries, including protocol implementation.

### PTP
PTP-specific libraries, including protocol implementation.

### Leaphash
Utility package for computing the hash value of the official leap-second.list document

### leapsectz
Utility package for obtaining leap second information from the system timezone database

### PHC
Library to work with PTP Hardware Clock (PHC).

### Timestamp
Library to work with NIC hardware/software timestamps.

### oscillatord
Implementation of monitoring protocol used by Orolia [oscillatord](https://github.com/Orolia2s/oscillatord).

### Calnex
Command line tool and library for a Calnex Sentinel device.

### Caliper
Command line tool for parsing Luciol OTDR trace files when measuring system/cable latencies of Huber-Suhner GNSS-over-fiber systems

### sa53
Command line tool and library for the Microchip SA53 atomic clock on Celestica time cards.

### fbclock
Client C library and Go daemon to provide TrueTime API based on PTP time.

### servo
Pi servo library

### clock
Wrapper around CLOCK_ADJTIME syscall

### NTRIP
NTRIP v1 SOURCE client library for pushing RTCM correction data to an NTRIP caster. Supports HTTP CONNECT proxy with TLS client certificate authentication.

### ntripper
Command line tool that reads RTCM3 and UBX-RXM-RAWX data from an oscillatord Unix socket and pushes RTCM corrections to an NTRIP caster, generating MSM7 messages from raw observations when available.

### RTCM
Parser and scanner for RTCM 10403.x (RTCM3) binary frames used for GNSS correction data. Includes CRC-24Q validation and stream synchronization.

# License
time is licensed under Apache 2.0 as found in the [LICENSE file](LICENSE).
