// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// AuthModeAPIKey is the class's only authentication mode: the Releem API key
// of the account the agents report to, in the credentials Secret's apiKey
// key.
const (
	AuthModeAPIKey   = "APIKey"
	CredentialAPIKey = "apiKey"
)

// Releem data storage regions.
const (
	RegionUS = "US"
	RegionEU = "EU"
)

// destinationParameters is MonitoringDestination.spec.parameters for a
// releem class.
type destinationParameters struct {
	// Region is where the Releem account stores its data: US (default) or EU.
	Region string `json:"region,omitempty"`
	// HostnamePrefix is put before every server name the agents report, to
	// tell clusters apart in one Releem account.
	HostnamePrefix string `json:"hostnamePrefix,omitempty"`
}

// instanceParameters is Instance.spec.monitoring.destinations[].parameters
// for a releem class.
type instanceParameters struct {
	// QueryOptimization makes the agents collect query samples and EXPLAIN
	// plans for Releem's query recommendations.
	QueryOptimization bool `json:"queryOptimization,omitempty"`
}

func decodeDestinationParameters(raw *runtime.RawExtension) (destinationParameters, error) {
	p := destinationParameters{}
	if raw != nil && len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &p); err != nil {
			return p, fmt.Errorf("decode parameters: %w", err)
		}
	}
	switch strings.ToUpper(p.Region) {
	case "", RegionUS:
		p.Region = RegionUS
	case RegionEU:
		p.Region = RegionEU
	default:
		return p, fmt.Errorf("region %q is not US or EU", p.Region)
	}
	if p.HostnamePrefix != "" {
		if errs := validation.IsDNS1123Label(strings.TrimSuffix(p.HostnamePrefix, "-")); len(errs) > 0 {
			return p, fmt.Errorf("hostnamePrefix %q: %s", p.HostnamePrefix, strings.Join(errs, "; "))
		}
	}
	return p, nil
}

func decodeInstanceParameters(raw *runtime.RawExtension) (instanceParameters, error) {
	p := instanceParameters{}
	if raw != nil && len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &p); err != nil {
			return p, fmt.Errorf("decode Instance parameters: %w", err)
		}
	}
	return p, nil
}
