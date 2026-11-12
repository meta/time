//go:build linux

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
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/facebook/time/fbclock"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func init() {
	RootCmd.AddCommand(newShmemCommand())
}

func newShmemCommand() *cobra.Command {
	var path string
	var jsonOut, v2 bool
	command := &cobra.Command{
		Use:          "shmem",
		Short:        "Print fbclock shared memory records",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(command *cobra.Command, _ []string) error {
			ConfigureVerbosity()
			dataPath := path
			if v2 && !command.Flags().Changed("path") {
				dataPath = fbclock.DataV2Path
			}
			data, err := fbclock.ReadFBClockDataFile(dataPath, v2)
			if err != nil {
				return err
			}
			if jsonOut {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(data)
			}
			return shmemPrintText(command.OutOrStdout(), data)
		},
	}
	command.Flags().StringVar(&path, "path", fbclock.DataPath, "shared memory file")
	command.Flags().BoolVarP(&jsonOut, "json", "j", false, "JSON output")
	command.Flags().BoolVar(&v2, "v2", false, "read the v2 file instead")
	return command
}

func shmemPrintText(w io.Writer, data *fbclock.DataFileSnapshot) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Path: %s\nVersion: %d\nSize: %d bytes\n", data.Path, data.Version, data.Size)
	if header := data.Header; header != nil {
		// the magic is a C string, ending at its first NUL; the reader rejects any other magic
		magic, _, _ := strings.Cut(header.Magic, "\x00")
		fmt.Fprintf(&b, "Header:\n  magic: %s\n  major: %d\n  minor: %d\n  sources: 0x%08x (%s)\n",
			magic, header.Major, header.Minor, header.Sources, shmemSourcesLabel(header.Sources))
	}
	for _, section := range data.Sections {
		fmt.Fprintf(&b, "\n%s (offset %d):\n  seq: %d\n", section.Name, section.Offset, section.Seq)
		if section.ReadError != "" {
			fmt.Fprintf(&b, "  read_error: %s\n", section.ReadError)
		}
		clock := section.Clock
		if clock == nil {
			continue
		}
		fmt.Fprintf(&b, "  clock_id: %d (%s)\n", clock.ClockID, shmemClockLabel(clock.ClockID))
		if clock.RefTimeNS != nil {
			fmt.Fprintf(&b, "  ref_time_ns: %d\n", *clock.RefTimeNS)
		}
		if clock.PHCTimeNS != nil {
			fmt.Fprintf(&b, "  phc_time_ns: %d\n", *clock.PHCTimeNS)
		}
		fmt.Fprintf(&b, "  sysclock_time_ns: %d\n  coef_ppb: %d\n  ingress_time_ns: %d\n  error_bound_ns: %d\n  holdover_multiplier_raw: %d\n  holdover_multiplier_ns: %g ns/s\n",
			clock.SysclockTimeNS, clock.CoefPPB, clock.IngressTimeNS, clock.ErrorBoundNS, clock.HoldoverMultiplierRaw, clock.HoldoverMultiplierNS)
		if clock.Reserved != nil {
			fmt.Fprintf(&b, "  reserved: %v\n", *clock.Reserved)
		}
		if clock.SmearingStartS != nil {
			fmt.Fprintf(&b, "  clock_smearing_start_s: %d\n", *clock.SmearingStartS)
		}
		if clock.UTCOffsetPreS != nil {
			fmt.Fprintf(&b, "  utc_offset_pre_s: %d\n", *clock.UTCOffsetPreS)
		}
		if clock.UTCOffsetPostS != nil {
			fmt.Fprintf(&b, "  utc_offset_post_s: %d\n", *clock.UTCOffsetPostS)
		}
		if section.AnchorAgeNS != nil {
			fmt.Fprintf(&b, "  anchor_age_ns: %d\n", *section.AnchorAgeNS)
		}
		if section.SinceIngressNS != nil {
			fmt.Fprintf(&b, "  since_ingress_ns: %d\n", *section.SinceIngressNS)
		}
		if section.AgeError != "" {
			fmt.Fprintf(&b, "  age_error: %s\n", section.AgeError)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func shmemSourcesLabel(sources uint32) string {
	var names []string
	for rest := sources; rest != 0; rest &= rest - 1 {
		names = append(names, fbclock.DataSectionName(fbclock.Sources(rest&-rest)))
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, "|")
}

func shmemClockLabel(clockID uint32) string {
	switch clockID {
	case unix.CLOCK_REALTIME:
		return "CLOCK_REALTIME"
	case unix.CLOCK_MONOTONIC:
		return "CLOCK_MONOTONIC"
	case unix.CLOCK_MONOTONIC_RAW:
		return "CLOCK_MONOTONIC_RAW"
	default:
		return "unknown"
	}
}
