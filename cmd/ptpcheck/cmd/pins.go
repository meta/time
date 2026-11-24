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
	"fmt"
	"os"
	"strings"

	"github.com/facebook/time/phc"
	"github.com/facebook/time/phc/unix" // a temporary shim for "golang.org/x/sys/unix" until v0.27.0 is cut
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// flags
var devPath string
var pinName string
var pinFunc PinFunc
var setMode bool

func init() {
	RootCmd.AddCommand(pinsCmd)
	pinsCmd.Flags().StringVarP(&devPath, "device", "d", "/dev/ptp0", "the PTP device")
	pinsCmd.Flags().StringVarP(&pinName, "name", "n", "", "name of the pin")
	pinsCmd.Flags().VarP(&pinFunc, "mode", "m", "the PTP function")
	pinsCmd.Flags().BoolVarP(&setMode, "set", "s", false, "set PTP function")
}

var pinsCmd = &cobra.Command{
	Use:   "pins",
	Short: "Print PHC pins and their functions",
	Run: func(_ *cobra.Command, _ []string) {
		ConfigureVerbosity()
		if setMode && pinName == "" {
			log.Fatal("-set needs -name")
		}
		if err := doListPins(devPath); err != nil {
			log.Fatal(err)
		}
	},
}

func doListPins(device string) error {
	// we may need RW permissions to issue PTP_SETFUNC ioctl on the device
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening device %q: %w", device, err)
	}
	defer f.Close()

	caps, err := unix.IoctlPtpClockGetcaps(int(f.Fd()))
	if err != nil {
		return err
	}
	npins := int(caps.N_pins)

	pins := make([]*unix.PtpPinDesc, npins)
	names := make([]string, npins)
	for i := 0; i < npins; i++ {
		pin, err := unix.IoctlPtpPinGetfunc(int(f.Fd()), uint(i)) //#nosec G115
		if err != nil {
			return err
		}
		pins[i] = pin
		names[i] = unix.ByteSliceToString(pin.Name[:])
	}

	for i, pin := range pins {
		if setMode && pinName == names[i] {
			pin.Func = uint32(pinFunc)
			if err := unix.IoctlPtpPinSetfunc(int(f.Fd()), pin); err != nil {
				return fmt.Errorf("%s: IoctlPtpPinSetfunc: %w", f.Name(), err)
			}
			if err := phc.EnablePinFunc(phc.FromFile(f), pin.Func, pin.Chan); err != nil {
				return fmt.Errorf("%s: enabling %s on %s: %w", f.Name(), PinFunc(pin.Func), names[i], err)
			}
		}
		if pinName == "" || pinName == names[i] {
			fmt.Printf("%s: pin %d function %-7[3]s (%[3]d) chan %d\n",
				pin.Name, pin.Index, PinFunc(pin.Func), pin.Chan)
		}
	}
	return nil
}

// PinFunc type represents the pin function values.
type PinFunc uint32

// Type implements cobra.Value
func (pf *PinFunc) Type() string { return "{ PPS-In | PPS-Out | PhySync | None }" }

// String implements flags.Value
func (pf PinFunc) String() string {
	switch pf {
	case unix.PTP_PF_NONE:
		return "None"
	case unix.PTP_PF_EXTTS:
		return "PPS-In" // user friendly
	case unix.PTP_PF_PEROUT:
		return "PPS-Out" // user friendly
	case unix.PTP_PF_PHYSYNC:
		return "PhySync"
	default:
		return fmt.Sprintf("!(PinFunc=%d)", int(pf))
	}
}

// Set implements flags.Value
func (pf *PinFunc) Set(s string) error {
	switch strings.ToLower(s) {
	case "none", "-":
		*pf = unix.PTP_PF_NONE
	case "pps-in", "ppsin", "extts":
		*pf = unix.PTP_PF_EXTTS
	case "pps-out", "ppsout", "perout":
		*pf = unix.PTP_PF_PEROUT
	case "phy-sync", "physync", "sync":
		*pf = unix.PTP_PF_PHYSYNC
	default:
		return fmt.Errorf("use either of: %s", pf.Type())
	}
	return nil
}
