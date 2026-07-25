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

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// IsolatedSessionAuthMode controls authorization for routes containing an
// isolated session ID. Legacy mode preserves the existing session-ID-only
// contract. Capability mode is an operator opt-in and fails closed.
type IsolatedSessionAuthMode string

const (
	IsolatedSessionAuthModeLegacy     IsolatedSessionAuthMode = "legacy"
	IsolatedSessionAuthModeCapability IsolatedSessionAuthMode = "capability"

	isolatedSessionAuthModeContextKey = "isolated-session-auth-mode"
)

// ParseIsolatedSessionAuthMode validates the operator-controlled auth mode.
func ParseIsolatedSessionAuthMode(value string) (IsolatedSessionAuthMode, error) {
	switch IsolatedSessionAuthMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", IsolatedSessionAuthModeLegacy:
		return IsolatedSessionAuthModeLegacy, nil
	case IsolatedSessionAuthModeCapability:
		return IsolatedSessionAuthModeCapability, nil
	default:
		return "", fmt.Errorf(
			"invalid isolated session auth mode %q: use %q or %q",
			value,
			IsolatedSessionAuthModeLegacy,
			IsolatedSessionAuthModeCapability,
		)
	}
}

// CapabilityRequired reports whether every session-scoped route requires the
// one-time capability returned by session creation.
func (m IsolatedSessionAuthMode) CapabilityRequired() bool {
	return m == IsolatedSessionAuthModeCapability
}

func (m IsolatedSessionAuthMode) valid() bool {
	return m == IsolatedSessionAuthModeLegacy ||
		m == IsolatedSessionAuthModeCapability
}

// WithIsolatedSessionAuthMode publishes the already-validated process mode to
// the isolated route group. Raw capability values are never stored in context.
func WithIsolatedSessionAuthMode(mode IsolatedSessionAuthMode) gin.HandlerFunc {
	if !mode.valid() {
		panic(fmt.Sprintf("invalid isolated session auth mode %q", mode))
	}
	return func(ctx *gin.Context) {
		ctx.Set(isolatedSessionAuthModeContextKey, mode)
		ctx.Next()
	}
}

// IsolatedSessionAuthModeFromContext returns the mode installed by the router.
func IsolatedSessionAuthModeFromContext(
	ctx *gin.Context,
) (IsolatedSessionAuthMode, bool) {
	value, ok := ctx.Get(isolatedSessionAuthModeContextKey)
	if !ok {
		return "", false
	}
	mode, ok := value.(IsolatedSessionAuthMode)
	return mode, ok && mode.valid()
}

// RequireIsolatedSessionCapability protects every non-delete route containing
// a session ID when capability mode is enabled. Legacy mode deliberately keeps
// the pre-existing behavior so operators can roll out SDK support before
// enabling enforcement.
func RequireIsolatedSessionCapability(ctx *gin.Context) {
	mode, ok := IsolatedSessionAuthModeFromContext(ctx)
	if !ok {
		abortIsolatedSessionAuthConfiguration(ctx)
		return
	}
	if !mode.CapabilityRequired() {
		ctx.Next()
		return
	}

	c := NewIsolatedSessionController(ctx)
	if !c.probed() {
		c.RespondError(
			http.StatusServiceUnavailable,
			model.ErrorCodeServiceUnavailable,
			"isolation unavailable",
		)
		ctx.Abort()
		return
	}

	capabilityValues := isolatedSessionCapabilityValues(ctx)
	if len(capabilityValues) != 1 {
		abortInvalidIsolatedSessionCapability(c, ctx)
		return
	}

	release, err := isolatedRunner.AcquireSessionOperation(
		ctx.Param("sessionId"),
		capabilityValues[0],
	)
	switch {
	case errors.Is(err, runtime.ErrContextNotFound),
		errors.Is(err, runtime.ErrSessionCapabilityInvalid):
		// Do not disclose whether the session ID exists.
		abortInvalidIsolatedSessionCapability(c, ctx)
		return
	case err != nil:
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			err.Error(),
		)
		ctx.Abort()
		return
	}

	defer release()
	ctx.Next()
}

// RequireIsolatedSessionDeleteCapability authenticates and atomically revokes
// operation admission before DELETE runs. In legacy mode it installs the same
// deletion closure without capability validation, preserving existing wire
// behavior while keeping the handler fail closed if middleware is omitted.
func RequireIsolatedSessionDeleteCapability(ctx *gin.Context) {
	mode, ok := IsolatedSessionAuthModeFromContext(ctx)
	if !ok {
		abortIsolatedSessionAuthConfiguration(ctx)
		return
	}

	c := NewIsolatedSessionController(ctx)
	if !c.probed() {
		c.RespondError(
			http.StatusServiceUnavailable,
			model.ErrorCodeServiceUnavailable,
			"isolation unavailable",
		)
		ctx.Abort()
		return
	}

	sessionID := ctx.Param("sessionId")
	if !mode.CapabilityRequired() {
		ctx.Set(
			isolatedSessionDeleteFuncKey,
			func() error {
				return isolatedRunner.DeleteIsolatedSession(sessionID)
			},
		)
		ctx.Next()
		return
	}

	capabilityValues := isolatedSessionCapabilityValues(ctx)
	if len(capabilityValues) != 1 {
		abortInvalidIsolatedSessionCapability(c, ctx)
		return
	}

	deleteSession, err := isolatedRunner.BeginDeleteIsolatedSession(
		sessionID,
		capabilityValues[0],
	)
	switch {
	case errors.Is(err, runtime.ErrContextNotFound),
		errors.Is(err, runtime.ErrSessionCapabilityInvalid):
		abortInvalidIsolatedSessionCapability(c, ctx)
		return
	case err != nil:
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			err.Error(),
		)
		ctx.Abort()
		return
	}

	ctx.Set(isolatedSessionDeleteFuncKey, deleteSession)
	ctx.Next()
}

func abortInvalidIsolatedSessionCapability(
	c *IsolatedSessionController,
	ctx *gin.Context,
) {
	c.RespondError(
		http.StatusForbidden,
		model.ErrorCodeSessionCapabilityInvalid,
		"invalid or missing isolated session capability",
	)
	ctx.Abort()
}

func isolatedSessionCapabilityValues(ctx *gin.Context) []string {
	var values []string
	for key, headerValues := range ctx.Request.Header {
		if strings.EqualFold(key, model.SessionCapabilityHeader) {
			values = append(values, headerValues...)
		}
	}
	return values
}

func abortIsolatedSessionAuthConfiguration(ctx *gin.Context) {
	c := NewIsolatedSessionController(ctx)
	c.RespondError(
		http.StatusInternalServerError,
		model.ErrorCodeRuntimeError,
		"isolated session auth mode is not configured",
	)
	ctx.Abort()
}
