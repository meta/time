# ntripper

ntripper reads RTCM3 and UBX-RXM-RAWX data from an oscillatord Unix socket and
pushes RTCM corrections to an NTRIP caster using the SOURCE protocol.

When RAWX observations are available it generates MSM7 messages for GPS,
GLONASS, Galileo, BeiDou, QZSS and NavIC with proper carrier phase from the raw
observations, bypassing the receiver's limited native RTCM engine.

## Usage

```
ntripper -caster <host:port> -username <mount> -password <secret> [flags]
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-socket` | `/run/oscillatord/rtcm.sock` | path to the oscillatord RTCM Unix socket |
| `-caster` | _(required)_ | NTRIP caster address (`host:port`) |
| `-mountpoint` | | NTRIP caster mountpoint (e.g. `/MOUNT01`) |
| `-username` | | NTRIP username (also used as mountpoint if `-mountpoint` not set) |
| `-password` | | NTRIP SOURCE password |
| `-useragent` | `NTRIP ntripper/1.0` | NTRIP source agent string |
| `-proxy` | | HTTP CONNECT proxy address (`host:port`) |
| `-proxy-cert` | | PEM certificate for proxy TLS authentication |
| `-proxy-key` | | PEM private key for proxy TLS authentication (defaults to `-proxy-cert`) |
| `-reconnect-interval` | `5s` | delay between reconnection attempts |
| `-monitoring-port` | `8891` | port for JSON monitoring HTTP server (0 to disable) |
| `-log-level` | `info` | log level (`debug`, `info`, `warn`, `error`) |
| `-ntrip-version` | `1` | NTRIP protocol version: 1 (SOURCE) or 2 (HTTP POST) |
| `-chunked` | `true` | NTRIP v2 only: send body using HTTP chunked transfer encoding |
| `-dry-run` | `false` | read frames from socket and print to stdout instead of pushing to caster |
| `-dump` | | tee the exact outgoing RTCM stream sent to the caster into this file |
| `-parse` | | validate the RTCM3 framing of a captured dump file and exit |
| `-rawx-stats` | `false` | read the socket and report the GNSS/signal breakdown of raw RAWX observations, then exit |

## Examples

```
ntripper -caster caster.example.com:2101 -username MyMount -password secret

ntripper -caster caster.example.com:2101 -username MyMount -password secret \
         -proxy proxy.example.com:8082 -proxy-cert /path/to/cert.pem
```
