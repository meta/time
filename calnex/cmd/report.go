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
	"time"

	"github.com/facebook/time/calnex/api"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(reportCmd)
	reportCmd.Flags().BoolVar(&insecureTLS, "insecureTLS", false, "Ignore TLS certificate errors")
	reportCmd.Flags().StringVar(&target, "device", "", "device to export problem report from")
	reportCmd.Flags().StringVar(&dir, "dir", "/tmp", "dir to save report")
	if err := reportCmd.MarkFlagRequired("device"); err != nil {
		log.Fatal(err)
	}
}

func report() error {
	api, err := api.NewAPI(target, insecureTLS, 2*time.Minute)
	if err != nil {
		return err
	}

	reportFileName, err := api.FetchProblemReport(dir)
	if err != nil {
		return err
	}

	log.Infof("Report is captured in: %s", reportFileName)

	return nil
}

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "get problem report",
	Run: func(_ *cobra.Command, _ []string) {
		if err := report(); err != nil {
			log.Fatal(err)
		}
	},
}
