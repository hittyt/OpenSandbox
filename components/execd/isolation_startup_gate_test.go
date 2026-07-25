// Copyright 2026 Alibaba Group Holding Ltd.
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

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
)

func TestDecideIsolationStartupGate(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		mode       controller.IsolatedSessionAuthMode
		token      string
		wantSecure bool
		wantError  string
	}{
		{
			name: "ordinary sandbox remains legacy",
			mode: controller.IsolatedSessionAuthModeLegacy,
		},
		{
			name:      "secure admission requires capability mode",
			enabled:   true,
			mode:      controller.IsolatedSessionAuthModeLegacy,
			token:     "token",
			wantError: "together",
		},
		{
			name:      "capability mode requires secure admission",
			mode:      controller.IsolatedSessionAuthModeCapability,
			token:     "token",
			wantError: "together",
		},
		{
			name:      "secure admission requires access token",
			enabled:   true,
			mode:      controller.IsolatedSessionAuthModeCapability,
			wantError: "EXECD_ACCESS_TOKEN",
		},
		{
			name:      "secure admission rejects padded access token",
			enabled:   true,
			mode:      controller.IsolatedSessionAuthModeCapability,
			token:     " token ",
			wantError: "canonical",
		},
		{
			name:       "secure admission succeeds",
			enabled:    true,
			mode:       controller.IsolatedSessionAuthModeCapability,
			token:      "token",
			wantSecure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, err := decideIsolationStartupGate(
				tt.enabled,
				tt.mode,
				tt.token,
			)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantSecure, gate.secure)
		})
	}
}

func TestSecureIsolationStartupGateFailsClosed(t *testing.T) {
	gate := isolationStartupGate{secure: true}

	require.ErrorContains(t, gate.requireProbe(false, "probe failed"), "probe failed")
	require.Error(t, gate.requireBackend(false))
	require.ErrorContains(
		t,
		gate.requireRunner(errors.New("runner failed")),
		"runner failed",
	)

	require.NoError(t, gate.requireProbe(true, ""))
	require.NoError(t, gate.requireBackend(true))
	require.NoError(t, gate.requireRunner(nil))
}

func TestLegacyIsolationStartupGateKeepsBestEffortBehavior(t *testing.T) {
	gate := isolationStartupGate{}

	require.NoError(t, gate.requireProbe(false, "probe failed"))
	require.NoError(t, gate.requireBackend(false))
	require.NoError(t, gate.requireRunner(errors.New("runner failed")))
}
