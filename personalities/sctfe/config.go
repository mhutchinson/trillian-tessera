// Copyright 2016 Google LLC. All Rights Reserved.
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

package sctfe

import (
	"crypto"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/certificate-transparency-go/x509"
	"github.com/transparency-dev/trillian-tessera/personalities/sctfe/configpb"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"
)

// ValidatedLogConfig represents the LogConfig with the information that has
// been successfully parsed as a result of validating it.
type ValidatedLogConfig struct {
	Config        *configpb.LogConfig
	PubKey        crypto.PublicKey
	PrivKey       proto.Message
	KeyUsages     []x509.ExtKeyUsage
	NotAfterStart *time.Time
	NotAfterLimit *time.Time
}

// LogConfigSetFromFile creates a slice of LogConfigSet options from the given
// filename, which should contain text or binary-encoded protobuf configuration
// data.
func LogConfigSetFromFile(filename string) (*configpb.LogConfigSet, error) {
	cfgBytes, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var cfg configpb.LogConfigSet
	if txtErr := prototext.Unmarshal(cfgBytes, &cfg); txtErr != nil {
		if binErr := proto.Unmarshal(cfgBytes, &cfg); binErr != nil {
			return nil, fmt.Errorf("failed to parse LogConfigSet from %q as text protobuf (%v) or binary protobuf (%v)", filename, txtErr, binErr)
		}
	}

	if len(cfg.Config) == 0 {
		return nil, errors.New("empty log config found")
	}
	return &cfg, nil
}

// validateLogConfig checks that a single log config is valid. In particular:
//   - A log has a private, and optionally a public key (both valid).
//   - Each of NotBeforeStart and NotBeforeLimit, if set, is a valid timestamp
//     proto. If both are set then NotBeforeStart <= NotBeforeLimit.
//   - Merge delays (if present) are correct.
//
// Returns the validated structures (useful to avoid double validation).
func validateLogConfig(cfg *configpb.LogConfig) (*ValidatedLogConfig, error) {
	if len(cfg.Origin) == 0 {
		return nil, errors.New("empty log origin")
	}

	vCfg := ValidatedLogConfig{Config: cfg}

	// Validate the public key.
	if pubKey := cfg.PublicKey; pubKey != nil {
		var err error
		if vCfg.PubKey, err = x509.ParsePKIXPublicKey(pubKey.Der); err != nil {
			return nil, fmt.Errorf("x509.ParsePKIXPublicKey: %w", err)
		}
	}

	// Validate the private key.
	if cfg.PrivateKey == nil {
		return nil, errors.New("empty private key")
	}
	privKey, err := cfg.PrivateKey.UnmarshalNew()
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}
	vCfg.PrivKey = privKey

	if cfg.RejectExpired && cfg.RejectUnexpired {
		return nil, errors.New("rejecting all certificates")
	}

	// validate storage config
	if cfg.StorageConfig == nil {
		return nil, errors.New("empty storage config")
	}

	// Validate the extended key usages list.
	if len(cfg.ExtKeyUsages) > 0 {
		for _, kuStr := range cfg.ExtKeyUsages {
			if ku, ok := stringToKeyUsage[kuStr]; ok {
				// If "Any" is specified, then we can ignore the entire list and
				// just disable EKU checking.
				if ku == x509.ExtKeyUsageAny {
					klog.Infof("%s: Found ExtKeyUsageAny, allowing all EKUs", cfg.Origin)
					vCfg.KeyUsages = nil
					break
				}
				vCfg.KeyUsages = append(vCfg.KeyUsages, ku)
			} else {
				return nil, fmt.Errorf("unknown extended key usage: %s", kuStr)
			}
		}
	}

	// Validate the time interval.
	start, limit := cfg.NotAfterStart, cfg.NotAfterLimit
	if start != nil {
		vCfg.NotAfterStart = &time.Time{}
		if err := start.CheckValid(); err != nil {
			return nil, fmt.Errorf("invalid start timestamp: %v", err)
		}
		*vCfg.NotAfterStart = start.AsTime()
	}
	if limit != nil {
		vCfg.NotAfterLimit = &time.Time{}
		if err := limit.CheckValid(); err != nil {
			return nil, fmt.Errorf("invalid limit timestamp: %v", err)
		}
		*vCfg.NotAfterLimit = limit.AsTime()
	}
	if start != nil && limit != nil && (*vCfg.NotAfterLimit).Before(*vCfg.NotAfterStart) {
		return nil, errors.New("limit before start")
	}

	switch {
	case cfg.MaxMergeDelaySec < 0:
		return nil, errors.New("negative maximum merge delay")
	case cfg.ExpectedMergeDelaySec < 0:
		return nil, errors.New("negative expected merge delay")
	case cfg.ExpectedMergeDelaySec > cfg.MaxMergeDelaySec:
		return nil, errors.New("expected merge delay exceeds MMD")
	}

	return &vCfg, nil
}

// ValidateLogConfigSet validate each configs independently and makes sure
// there aren't any duplicate entries.
func ValidateLogConfigSet(cfg *configpb.LogConfigSet) ([]*ValidatedLogConfig, error) {
	logNameMap := make(map[string]bool)
	ret := []*ValidatedLogConfig{}
	for _, logCfg := range cfg.Config {
		vcfg, err := validateLogConfig(logCfg)
		if err != nil {
			return nil, fmt.Errorf("log config: %v: %v", err, logCfg)
		}
		if logNameMap[logCfg.Origin] {
			return nil, fmt.Errorf("log config: duplicate origin: %s: %v", logCfg.Origin, logCfg)
		}
		logNameMap[logCfg.Origin] = true
		ret = append(ret, vcfg)
	}

	return ret, nil
}

var stringToKeyUsage = map[string]x509.ExtKeyUsage{
	"Any":                        x509.ExtKeyUsageAny,
	"ServerAuth":                 x509.ExtKeyUsageServerAuth,
	"ClientAuth":                 x509.ExtKeyUsageClientAuth,
	"CodeSigning":                x509.ExtKeyUsageCodeSigning,
	"EmailProtection":            x509.ExtKeyUsageEmailProtection,
	"IPSECEndSystem":             x509.ExtKeyUsageIPSECEndSystem,
	"IPSECTunnel":                x509.ExtKeyUsageIPSECTunnel,
	"IPSECUser":                  x509.ExtKeyUsageIPSECUser,
	"TimeStamping":               x509.ExtKeyUsageTimeStamping,
	"OCSPSigning":                x509.ExtKeyUsageOCSPSigning,
	"MicrosoftServerGatedCrypto": x509.ExtKeyUsageMicrosoftServerGatedCrypto,
	"NetscapeServerGatedCrypto":  x509.ExtKeyUsageNetscapeServerGatedCrypto,
}
