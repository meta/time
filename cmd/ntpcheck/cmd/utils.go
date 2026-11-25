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

package cmd

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/facebook/time/leaphash"
	"github.com/facebook/time/ntp/chrony"
	ntp "github.com/facebook/time/ntp/protocol"
	"github.com/facebook/time/timestamp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// refID converts ip into ReFID format and prints it on stdout
func refID(ipStr string) error {
	if ipStr == "" {
		return fmt.Errorf("no IP provided")
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("%q is not a valid IP address", ipStr)
	}

	// output IPv4 as-is
	ipv4 := ip.To4()
	if ipv4 != nil {
		fmt.Println(ipv4)
		return nil
	}

	hashed := md5.Sum(ip)
	fmt.Println(net.IPv4(hashed[0], hashed[1], hashed[2], hashed[3]).String())
	return nil
}

// fakeSeconds displays N fake leap seconds on stdout
func fakeSeconds(secondsCount int) {
	ntpEpoch := time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC)
	now := time.Now()
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// the timestamp in this format is number of seconds from NTP Epoch
	fmt.Printf("# Generating %d fake leap seconds:\n", secondsCount)
	for i := range secondsCount {
		firstOfFakeMonth := firstOfThisMonth.AddDate(0, i+1, 0)
		delta := int((firstOfFakeMonth.Sub(ntpEpoch)).Seconds())
		fmt.Printf("%d  XX  # %s\n", delta, firstOfFakeMonth.Format(time.RFC3339))
	}
}

// signFile generates hash for leap Second hashfile and prints is on stdout
func signFile(fileName string) {
	data, err := os.ReadFile(fileName)
	if err != nil {
		fmt.Printf("Error opening %q: %s\n", fileName, err)
	}

	fmt.Printf("#h %s\n", leaphash.Compute(string(data)))
}

func stripZeroes(num float64) string {
	s := fmt.Sprintf("%.2f", num)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// receiveNTPPacketWithRetries receives NTP packet from the socket and returns it.
// In the perfect world we simply set socket to blocking mode and read from it, which blocks until we have a packet.
// However currently there is a bug in the kernel which causes a race between receiving packet and unblocking recvmsg syscall,
// which then can block forever.
// To work around this, we are using socket-level timeouts and retrying to read packet.
func receiveNTPPacketWithRetries(connFd int, buf, oob []byte, tries int) (*ntp.Packet, time.Time, error) {
	var err error
	var n int
	var clientReceiveTime time.Time
	for range tries {
		n, _, clientReceiveTime, err = timestamp.ReadPacketWithRXTimestampBuf(connFd, buf, oob)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			log.Debug("got timeout reading response packet, retrying")
			continue
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, clientReceiveTime, err
	}
	response, err := ntp.BytesToPacket(buf[:n])
	if err != nil {
		return nil, clientReceiveTime, err
	}
	return response, clientReceiveTime, nil
}

// ntpDate prints data similar to 'ntptime' command output
func ntpDate(remoteServerAddr string, remoteServerPort string, requests int, ntpdateLocalAddr string) (err error) {
	timeout := 5 * time.Second
	singleAttemptTimeout := 100 * time.Millisecond
	tries := int(timeout / singleAttemptTimeout)
	addr := net.JoinHostPort(remoteServerAddr, remoteServerPort)

	dialer := &net.Dialer{
		LocalAddr: &net.UDPAddr{
			// nil IP is same as default
			IP: net.ParseIP(ntpdateLocalAddr),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	defer conn.Close()

	// get connection file descriptor
	connFd, err := timestamp.ConnFd(conn.(*net.UDPConn))
	if err != nil {
		return err
	}

	// Allow reading of kernel timestamps via socket
	if err := timestamp.EnableSWTimestampsRx(connFd); err != nil {
		return err
	}
	err = unix.SetNonblock(connFd, false)
	if err != nil {
		return err
	}

	var sumDelay int64
	var sumOffset int64
	var received int
	var i int
	var skipped int
	// report how many packets were skipped, and how many processed if we encounter an error
	defer func() {
		if err != nil {
			log.Warningf("Processed %d requests before error", i)
		}
		if skipped > 0 {
			log.Warningf("total %d packets skipped", skipped)
		}
	}()
	// set socket level timeout to work around a kernel bug when recvmsg blocks forever.
	// see receiveNTPPacketWithRetries for details
	err = setSocketTimeout(connFd, singleAttemptTimeout)
	if err != nil {
		return err
	}

	// buffers
	buf := make([]byte, 1024)
	oob := make([]byte, 1024)

	for i = 0; i < requests; i++ {
		clientTransmitTime := time.Now()
		sec, frac := ntp.Time(clientTransmitTime)
		clientWireTransmitTime := ntp.Unix(sec, frac)
		log.Debugf("Client TX timestamp (ntp): %v\n", clientWireTransmitTime)

		request := &ntp.Packet{
			Settings:   0x1B,
			TxTimeSec:  sec,
			TxTimeFrac: frac,
		}

		b, err := request.Bytes()
		if err != nil {
			return fmt.Errorf("failed to marshal request, %w", err)
		}
		if _, err := conn.Write(b); err != nil {
			return fmt.Errorf("failed to send request, %w", err)
		}
		response, clientReceiveTime, err := receiveNTPPacketWithRetries(connFd, buf, oob, tries)
		if err != nil {
			log.Errorf("Error reading response to %d after %d tries, err = %v", i, tries, err)
			continue
		}

		serverReceiveTime := ntp.Unix(response.RxTimeSec, response.RxTimeFrac)
		serverTransmitTime := ntp.Unix(response.TxTimeSec, response.TxTimeFrac)
		originTime := ntp.Unix(response.OrigTimeSec, response.OrigTimeFrac)

		log.Debugf("Origin TX timestamp (T1): %v", originTime)
		log.Debugf("Server RX timestamp (T2): %v", serverReceiveTime)
		log.Debugf("Server TX timestamp (T3): %v", serverTransmitTime)
		log.Debugf("Client RX timestamp (T4): %v", clientReceiveTime)

		// sanity check: origin time must be same as client transmit time
		// if it is not so, we probably have extra packet in the kernel queue which we need to read and discard
		if response.OrigTimeSec != sec || response.OrigTimeFrac != frac {
			response, clientReceiveTime, err = receiveNTPPacketWithRetries(connFd, buf, oob, tries)
			if err != nil {
				log.Errorf("Client TX timestamp %v not equal to Origin TX timestamp %v", clientTransmitTime, originTime)
				return err
			}
			log.Debug("skipped one packet from the receive queue")
			skipped++
			serverReceiveTime = ntp.Unix(response.RxTimeSec, response.RxTimeFrac)
			serverTransmitTime = ntp.Unix(response.TxTimeSec, response.TxTimeFrac)
			originTime = ntp.Unix(response.OrigTimeSec, response.OrigTimeFrac)
		}

		delay := ntp.RoundTripDelay(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime)
		offset := ntp.Offset(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime)
		correctTime := ntp.CorrectTime(clientReceiveTime, offset)

		sumDelay += delay
		sumOffset += offset
		received++

		if i == requests-1 {
			fmt.Printf("\nServer: %s, Stratum: %d, Reference ID: %s, Requests %d\n", addr, response.Stratum, chrony.RefidToString(response.ReferenceID), requests)
			fmt.Print("Last Request:")
			fmt.Printf("Offset: %fs (%sus) | Delay: %fs (%sus)\n",
				float64(offset)/float64(time.Second.Nanoseconds()),
				stripZeroes(math.Round(float64(offset)/float64(time.Microsecond.Nanoseconds()))),
				float64(delay)/float64(time.Second.Nanoseconds()),
				stripZeroes(math.Round(float64(delay)/float64(time.Microsecond.Nanoseconds()))))
			fmt.Printf("Correct Time is %s\n\n", correctTime)
		}
	}
	if received == 0 {
		return fmt.Errorf("no valid responses from %s to %d requests", addr, requests)
	}
	avgDelay := float64(sumDelay) / float64(received)
	avgOffset := float64(sumOffset) / float64(received)
	fmt.Printf("Average (%d requests):\n", received)
	fmt.Printf("Offset: %fs (%sus) | Delay: %fs (%sus)\n",
		avgOffset/float64(time.Second.Nanoseconds()),
		stripZeroes(math.Round(avgOffset/float64(time.Microsecond.Nanoseconds()))),
		avgDelay/float64(time.Second.Nanoseconds()),
		stripZeroes(math.Round(avgDelay/float64(time.Microsecond.Nanoseconds()))))

	return nil
}

// cli vars
var refidIP string
var fsCount int
var signFileName string
var remoteServerAddr string
var remoteServerPort int
var ntpdateRequests int
var ntpdateLocalAddr string

func init() {
	RootCmd.AddCommand(utilsCmd)
	// refid
	utilsCmd.AddCommand(refidCmd)
	refidCmd.Flags().StringVarP(&refidIP, "ip", "i", "", "IP address")
	// fakeseconds
	utilsCmd.AddCommand(fakeSecondsCmd)
	fakeSecondsCmd.Flags().IntVarP(&fsCount, "count", "c", 5, "Number of entities to generate")
	// signfile
	utilsCmd.AddCommand(signFileCmd)
	signFileCmd.Flags().StringVarP(&signFileName, "file", "f", "leap-seconds.list", "File name")
	// ntpdate
	utilsCmd.AddCommand(ntpdateCmd)
	ntpdateCmd.Flags().StringVarP(&remoteServerAddr, "server", "s", "", "Server to query")
	ntpdateCmd.Flags().IntVarP(&remoteServerPort, "port", "p", 123, "Port of the remote server")
	ntpdateCmd.Flags().IntVarP(&ntpdateRequests, "requests", "r", 3, "How many requests to send")
	ntpdateCmd.Flags().StringVarP(&ntpdateLocalAddr, "local-addr", "l", "", "Source IP address")
}

var utilsCmd = &cobra.Command{
	Use:   "utils",
	Short: "Collection of NTP-related utils",
}

var refidCmd = &cobra.Command{
	Use:   "refid",
	Short: "Converts IP address to refid",
	Run: func(_ *cobra.Command, _ []string) {
		ConfigureVerbosity()
		err := refID(refidIP)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	},
}

var fakeSecondsCmd = &cobra.Command{
	Use:   "fakeseconds",
	Short: "Prints some fake seconds.",
	Long:  `Prints some fake seconds (potential slots when leap seconds might happen in leap-seconds format. For reference: https://www.ietf.org/timezones/data/leap-seconds.list`,
	Run: func(_ *cobra.Command, _ []string) {
		ConfigureVerbosity()
		fakeSeconds(fsCount)
	},
}

var signFileCmd = &cobra.Command{
	Use:   "signfile",
	Short: "Generate hash signature for leap-seconds.list",
	Run: func(_ *cobra.Command, _ []string) {
		ConfigureVerbosity()
		signFile(signFileName)
	},
}

var ntpdateCmd = &cobra.Command{
	Use:   "ntpdate",
	Short: "Sends NTP request(s) to a remote NTP server. Similar to ntpdate -q",
	Long:  "'ntpdate' will poll remote NTP server and will report metrics including local clock offset and roundtrip delay based on response from server",
	Run: func(_ *cobra.Command, _ []string) {
		ConfigureVerbosity()
		if remoteServerAddr == "" {
			fmt.Println("server must be specified")
			os.Exit(1)
		}
		if err := ntpDate(remoteServerAddr, strconv.Itoa(remoteServerPort), ntpdateRequests, ntpdateLocalAddr); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	},
}
