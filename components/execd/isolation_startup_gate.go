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
	"fmt"
	"strings"

	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
)

// isolationStartupGate represents server-owned admission for secure isolated
// sessions. Legacy mode intentionally preserves the existing best-effort
// startup path for ordinary sandboxes.
type isolationStartupGate struct {
	secure bool
}

func decideIsolationStartupGate(
	isolationEnabled bool,
	authMode controller.IsolatedSessionAuthMode,
	accessToken string,
) (isolationStartupGate, error) {
	capabilityMode := authMode.CapabilityRequired()
	if isolationEnabled != capabilityMode {
		return isolationStartupGate{}, fmt.Errorf(
			"secure isolated sessions require EXECD_ISOLATION_ENABLED=true " +
				"and EXECD_SESSION_AUTH_MODE=capability together",
		)
	}
	if !isolationEnabled {
		return isolationStartupGate{}, nil
	}
	if accessToken == "" || strings.TrimSpace(accessToken) != accessToken {
		return isolationStartupGate{}, fmt.Errorf(
			"secure isolated sessions require a non-empty canonical EXECD_ACCESS_TOKEN",
		)
	}
	return isolationStartupGate{secure: true}, nil
}

func (g isolationStartupGate) requireProbe(
	available bool,
	message string,
) error {
	if !g.secure || available {
		return nil
	}
	if message == "" {
		message = "no isolation backend is available"
	}
	return fmt.Errorf(
		"secure isolated-session startup probe failed: %s",
		message,
	)
}

func (g isolationStartupGate) requireBackend(available bool) error {
	if !g.secure || available {
		return nil
	}
	return fmt.Errorf(
		"secure isolated-session lifecycle backend is unavailable",
	)
}

func (g isolationStartupGate) requireRunner(err error) error {
	if !g.secure || err == nil {
		return nil
	}
	return fmt.Errorf(
		"secure isolated-session runner initialization failed: %w",
		err,
	)
}
