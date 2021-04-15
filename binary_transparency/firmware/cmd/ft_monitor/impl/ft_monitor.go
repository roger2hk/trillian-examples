// Copyright 2020 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package impl is the implementation of the Firmware Transparency monitor.
// The monitor follows the growth of the Firmware Transparency log server,
// inspects new firmware metadata as it appears, and prints out a short
// summary.
//
// TODO(al): Extend monitor to verify claims.
package impl

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/golang/glog"
	"github.com/google/trillian-examples/binary_transparency/firmware/api"
	"github.com/google/trillian-examples/binary_transparency/firmware/internal/client"
	"github.com/google/trillian-examples/binary_transparency/firmware/internal/crypto"
)

// MatchFunc is the signature of a function which can be called by the monitor
// to signal when it's found a keyword match in a logged entry.
type MatchFunc func(index uint64, fw api.FirmwareMetadata)

// MonitorOpts encapsulates options for running the monitor.
type MonitorOpts struct {
	LogURL       string
	PollInterval time.Duration
	Keyword      string
	Matched      MatchFunc
	Annotate     bool
}

func Main(ctx context.Context, opts MonitorOpts) error {
	if len(opts.LogURL) == 0 {
		return errors.New("log URL is required")
	}

	ftURL, err := url.Parse(opts.LogURL)
	if err != nil {
		return fmt.Errorf("failed to parse FT log URL: %w", err)
	}

	// Parse the input keywords as regular expression
	matcher := regexp.MustCompile(opts.Keyword)

	c := client.ReadonlyClient{LogURL: ftURL}

	// TODO(mhutchinson): This checkpoint and tracker for number of processed entries
	// should be serialized so the monitor persists its golden state between runs.
	// In particular this will prevent the annotator from adding multiple annotations
	// for the same firmware each time the monitor is started.
	var latestCP api.LogCheckpoint
	var head uint64
	follow := client.NewLogFollower(c)

	glog.Infof("Monitoring FT log %q...", opts.LogURL)
	cpc, cperrc := follow.Checkpoints(ctx, opts.PollInterval, latestCP)
	ec, eerrc := follow.Entries(ctx, cpc, head)

	for {
		var entry client.LogEntry
		select {
		case err = <-cperrc:
			return err
		case err = <-eerrc:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case entry = <-ec:
		}

		stmt := entry.Value
		if stmt.Type != api.FirmwareMetadataType {
			// Only analyze firmware statements in the monitor.
			continue
		}

		// Parse the firmware metadata:
		var meta api.FirmwareMetadata
		if err := json.Unmarshal(stmt.Statement, &meta); err != nil {
			glog.Warningf("Unable to decode FW Metadata from Statement %q", err)
			continue
		}

		glog.Infof("Found firmware (@%d): %s", entry.Index, meta)

		// Fetch the Image from FT Personality
		image, err := c.GetFirmwareImage(meta.FirmwareImageSHA512)
		if err != nil {
			glog.Warningf("Unable to GetFirmwareImage for Firmware with Hash 0x%x , reason %q", meta.FirmwareImageSHA512, err)
			continue
		}
		// Verify Image Hash from log Manifest matches the actual image hash
		h := sha512.Sum512(image)
		if !bytes.Equal(h[:], meta.FirmwareImageSHA512) {
			glog.Warningf("downloaded image does not match SHA512 in metadata (%x != %x)", h[:], meta.FirmwareImageSHA512)
			continue
		}
		glog.V(1).Infof("Image Hash Verified for image at leaf index %d", entry.Index)

		// Search for specific keywords inside firmware image
		malwareDetected := matcher.Match(image)

		if malwareDetected {
			opts.Matched(entry.Index, meta)
		}
		if opts.Annotate {
			ms := api.MalwareStatement{
				FirmwareID: api.FirmwareID{
					LogIndex:            entry.Index,
					FirmwareImageSHA512: h[:],
				},
				Good: !malwareDetected,
			}
			glog.V(1).Infof("Annotating %s", ms)
			js, err := createStatementJSON(ms)
			if err != nil {
				return fmt.Errorf("failed to create annotation: %w", err)
			}
			sc := client.SubmitClient{
				ReadonlyClient: &c,
			}
			if err := sc.PublishAnnotationMalware(js); err != nil {
				return fmt.Errorf("failed to publish annotation: %w", err)
			}
		}
	}
}

func createStatementJSON(m api.MalwareStatement) ([]byte, error) {
	js, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}
	sig, err := crypto.AnnotatorMalware.SignMessage(api.MalwareStatementType, js)
	if err != nil {
		return nil, fmt.Errorf("failed to generate signature: %w", err)
	}

	statement := api.SignedStatement{
		Type:      api.MalwareStatementType,
		Statement: js,
		Signature: sig,
	}

	return json.Marshal(statement)
}