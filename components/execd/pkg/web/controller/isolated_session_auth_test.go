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

package controller

import "testing"

func TestParseIsolatedSessionAuthMode(t *testing.T) {
	tests := []struct {
		name               string
		value              string
		want               IsolatedSessionAuthMode
		capabilityRequired bool
		wantError          bool
	}{
		{
			name:  "empty defaults to legacy",
			value: "",
			want:  IsolatedSessionAuthModeLegacy,
		},
		{
			name:  "legacy",
			value: "legacy",
			want:  IsolatedSessionAuthModeLegacy,
		},
		{
			name:  "legacy is case insensitive and trimmed",
			value: "  LEGACY  ",
			want:  IsolatedSessionAuthModeLegacy,
		},
		{
			name:               "capability",
			value:              "capability",
			want:               IsolatedSessionAuthModeCapability,
			capabilityRequired: true,
		},
		{
			name:               "capability is case insensitive and trimmed",
			value:              "  CAPABILITY  ",
			want:               IsolatedSessionAuthModeCapability,
			capabilityRequired: true,
		},
		{
			name:      "unknown mode fails closed",
			value:     "optional",
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode, err := ParseIsolatedSessionAuthMode(test.value)
			if test.wantError {
				if err == nil {
					t.Fatalf("ParseIsolatedSessionAuthMode(%q) returned no error", test.value)
				}
				if mode != "" {
					t.Fatalf("mode = %q, want empty on error", mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIsolatedSessionAuthMode(%q): %v", test.value, err)
			}
			if mode != test.want {
				t.Fatalf("mode = %q, want %q", mode, test.want)
			}
			if got := mode.CapabilityRequired(); got != test.capabilityRequired {
				t.Fatalf(
					"CapabilityRequired() = %v, want %v",
					got,
					test.capabilityRequired,
				)
			}
		})
	}
}
