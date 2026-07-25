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
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/telemetry"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

// isolatedRunner is set by InitIsolatedRunner during startup.
var isolatedRunner *runtime.IsolatedRunner

// isolatedProbeResult stores the probe result for capabilities reporting.
var isolatedProbeResult *isolation.ProbeResult

const isolatedSessionDeleteFuncKey = "isolated-session-delete-func"

// InitIsolatedRunner wires the isolated session runner.
func InitIsolatedRunner(r *runtime.IsolatedRunner) {
	isolatedRunner = r
}

// InitIsolatedProbe stores the probe result for the capabilities endpoint.
func InitIsolatedProbe(p *isolation.ProbeResult) {
	isolatedProbeResult = p
}

// IsolatedSessionController handles /v1/isolated/* endpoints.
type IsolatedSessionController struct {
	*basicController
}

// NewIsolatedSessionController creates a controller bound to ctx.
func NewIsolatedSessionController(ctx *gin.Context) *IsolatedSessionController {
	return &IsolatedSessionController{
		basicController: newBasicController(ctx),
	}
}

func (c *IsolatedSessionController) probed() bool {
	return isolatedRunner != nil && isolatedRunner.Available()
}

// Create handles POST /v1/isolated/session.
func (c *IsolatedSessionController) Create() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}
	mode, ok := IsolatedSessionAuthModeFromContext(c.ctx)
	if !ok {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			"isolated session auth mode is not configured",
		)
		return
	}

	var req model.CreateIsolatedSessionRequest
	if err := c.bindJSON(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}
	if mode.CapabilityRequired() {
		switch {
		case req.ShareNet == nil:
			// The hardened omitted/default path requires the one-to-one
			// network backend and mandatory guard. Until those are ready,
			// fail closed before any workload or filesystem side effect.
			c.RespondError(
				http.StatusServiceUnavailable,
				model.ErrorCodeSessionNetworkBackendUnavailable,
				"secure default session network backend is unavailable",
			)
			return
		case *req.ShareNet:
			c.RespondError(
				http.StatusBadRequest,
				model.ErrorCodeSessionSharedNetworkForbidden,
				"shared network is forbidden for secure isolated sessions",
			)
			return
		}
	}

	binds := make([]isolation.BindMount, 0, len(req.Binds))
	for _, b := range req.Binds {
		binds = append(binds, isolation.BindMount{
			Source:   b.Source,
			Dest:     b.Dest,
			ReadOnly: b.ReadOnly,
		})
	}

	opts := &runtime.IsolatedSessionOptions{
		Profile:            req.Profile,
		WorkspacePath:      req.Workspace.Path,
		WorkspaceMode:      req.Workspace.Mode,
		ExtraWritable:      req.ExtraWritable,
		Binds:              binds,
		ShareNet:           req.ShareNet,
		EnvPassthroughMode: req.EnvPassthrough.Mode,
		EnvPassthroughKeys: req.EnvPassthrough.Keys,
		Uid:                req.Uid,
		Gid:                req.Gid,
		UidMode:            req.UidMode,
		IdleTimeoutSeconds: req.IdleTimeoutSeconds,
	}

	sessionID, capability, err := isolatedRunner.CreateIsolatedSessionWithCapability(opts)
	if err != nil {
		status, code := classifyIsolatedCreateError(err)
		c.RespondError(status, code, err.Error())
		return
	}

	// The capability is a one-time credential. Prevent intermediaries and
	// browser caches from retaining the create response.
	c.ctx.Header("Cache-Control", "no-store")
	response := model.IsolatedCreateSessionResponse{
		SessionID: sessionID,
		CreatedAt: time.Now(),
	}
	if mode.CapabilityRequired() {
		response.Capability = capability
	}
	c.ctx.JSON(http.StatusCreated, response)
}

func classifyIsolatedCreateError(err error) (int, model.ErrorCode) {
	if errors.Is(err, runtime.ErrUidModeUnavailable) {
		return http.StatusServiceUnavailable, model.ErrorCodeNotSupported
	}
	if strings.Contains(err.Error(), "not in allowlist") ||
		strings.Contains(err.Error(), "not allowed") ||
		strings.Contains(err.Error(), "unknown isolation profile") ||
		strings.Contains(err.Error(), "must be an existing path") ||
		strings.Contains(err.Error(), "must be an absolute path") ||
		strings.Contains(err.Error(), "source is required") {
		return http.StatusBadRequest, model.ErrorCodeRuntimeError
	}
	return http.StatusInternalServerError, model.ErrorCodeRuntimeError
}

// Get handles GET /v1/isolated/session/:sessionId.
func (c *IsolatedSessionController) Get() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	sessionID := c.ctx.Param("sessionId")
	state, err := isolatedRunner.GetIsolatedSession(sessionID)
	if err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	resp := model.SessionState{
		Status:               state.Status,
		CreatedAt:            state.CreatedAt,
		LastRunAt:            state.LastRunAt,
		IdleRemainingSeconds: state.IdleRemainingSeconds,

		Profile:       state.Profile,
		ExtraWritable: state.ExtraWritable,
		ShareNet:      state.ShareNet,
		Uid:           state.Uid,
		Gid:           state.Gid,
		UidMode:       state.UidMode,
	}
	if state.WorkspacePath != "" {
		resp.Workspace = &model.WorkspaceSpec{
			Path: state.WorkspacePath,
			Mode: state.WorkspaceMode,
		}
	}
	if len(state.Binds) > 0 {
		resp.Binds = make([]model.BindMount, 0, len(state.Binds))
		for _, b := range state.Binds {
			resp.Binds = append(resp.Binds, model.BindMount{
				Source:   b.Source,
				Dest:     b.Dest,
				ReadOnly: b.ReadOnly,
			})
		}
	}
	if state.EnvPassthroughMode != "" || len(state.EnvPassthroughKeys) > 0 {
		resp.EnvPassthrough = &model.EnvPassthroughSpec{
			Mode: state.EnvPassthroughMode,
			Keys: state.EnvPassthroughKeys,
		}
	}
	// Echo idle_timeout_seconds unconditionally. A value of 0 is meaningful:
	// it means the session was created with idle GC disabled — the exact
	// configuration a stateless caller doing long-window recovery needs to
	// see. Older execd builds that don't set this field are distinguished
	// by the pointer being nil.
	idle := state.IdleTimeoutSeconds
	resp.IdleTimeoutSeconds = &idle
	c.RespondSuccess(resp)
}

// List handles GET /v1/isolated/sessions.
func (c *IsolatedSessionController) List() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}
	mode, ok := IsolatedSessionAuthModeFromContext(c.ctx)
	if !ok {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			"isolated session auth mode is not configured",
		)
		return
	}
	if mode.CapabilityRequired() {
		c.RespondError(
			http.StatusForbidden,
			model.ErrorCodeSessionListForbidden,
			"isolated session listing is disabled in capability mode",
		)
		return
	}

	sessions := isolatedRunner.ListIsolatedSessions()
	items := make([]model.IsolatedSessionSummary, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, model.IsolatedSessionSummary{
			SessionID:            s.SessionID,
			Status:               s.Status,
			CreatedAt:            s.CreatedAt,
			LastRunAt:            s.LastRunAt,
			IdleRemainingSeconds: s.IdleRemainingSeconds,
		})
	}

	c.RespondSuccess(model.ListIsolatedSessionsResponse{Sessions: items})
}

// Run handles POST /v1/isolated/session/:sessionId/run (SSE streaming).
func (c *IsolatedSessionController) Run() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	sessionID := c.ctx.Param("sessionId")

	var req model.IsolatedRunRequest
	if err := c.bindJSON(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, err.Error())
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if req.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(c.ctx.Request.Context(), time.Duration(req.TimeoutSeconds)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(c.ctx.Request.Context())
	}
	defer cancel()

	// SSE stdout callback.
	onStdout := func(line string) {
		if line == "" {
			return
		}
		event := model.ServerStreamEvent{
			Type:      model.StreamEventTypeStdout,
			Text:      line,
			Timestamp: time.Now().UnixMilli(),
		}
		c.writeSingleEvent("IsolatedStdout", event.ToJSON(), false, event.Summary())
	}

	startTime := time.Now()
	err := isolatedRunner.RunInIsolatedSession(ctx, sessionID, req.Code, req.Envs, onStdout)
	durationMs := float64(time.Since(startTime)) / float64(time.Millisecond)

	if err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return
		}
		telemetry.RecordIsolatedRun(ctx, "error", durationMs)
		ename := "RuntimeError"
		evalue := err.Error()
		if strings.HasPrefix(evalue, "command exited with code ") {
			ename = "ExitError"
			evalue = strings.TrimPrefix(evalue, "command exited with code ")
		}
		event := model.ServerStreamEvent{
			Type:      model.StreamEventTypeError,
			Text:      err.Error(),
			Timestamp: time.Now().UnixMilli(),
			Error: &execute.ErrorOutput{
				EName:  ename,
				EValue: evalue,
			},
		}
		c.writeSingleEvent("IsolatedError", event.ToJSON(), true, event.Summary())
		return
	}
	telemetry.RecordIsolatedRun(ctx, "success", durationMs)
	event := model.ServerStreamEvent{
		Type:      model.StreamEventTypeComplete,
		Timestamp: time.Now().UnixMilli(),
	}
	c.writeSingleEvent("IsolatedComplete", event.ToJSON(), true, event.Summary())
}

// Delete handles DELETE /v1/isolated/session/:sessionId.
func (c *IsolatedSessionController) Delete() {
	if !c.probed() {
		c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeServiceUnavailable, "isolation unavailable")
		return
	}

	deleteValue, ok := c.ctx.Get(isolatedSessionDeleteFuncKey)
	if !ok {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			"isolated session delete authorization is not configured",
		)
		return
	}
	deleteSession, ok := deleteValue.(func() error)
	if !ok || deleteSession == nil {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			"isolated session delete authorization is invalid",
		)
		return
	}

	if err := deleteSession(); err != nil {
		if errors.Is(err, runtime.ErrContextNotFound) {
			c.RespondError(http.StatusNotFound, model.ErrorCodeSessionNotFound, "session not found")
			return
		}
		if errors.Is(err, runtime.ErrSessionTeardownTimeout) {
			c.RespondError(
				http.StatusInternalServerError,
				model.ErrorCodeSessionTeardownTimeout,
				"isolated session teardown timed out; terminate the sandbox",
			)
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}

	c.RespondSuccess(nil)
}

// Diff handles GET /v1/isolated/session/:sessionId/diff.
func (c *IsolatedSessionController) Diff() {
	c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeNotSupported, "diff not implemented yet (phase 2)")
}

// Commit handles POST /v1/isolated/session/:sessionId/commit.
func (c *IsolatedSessionController) Commit() {
	c.RespondError(http.StatusServiceUnavailable, model.ErrorCodeNotSupported, "commit not implemented yet (phase 2)")
}

// Capabilities handles GET /v1/isolated/capabilities.
func (c *IsolatedSessionController) Capabilities() {
	mode, ok := IsolatedSessionAuthModeFromContext(c.ctx)
	if !ok {
		c.RespondError(
			http.StatusInternalServerError,
			model.ErrorCodeRuntimeError,
			"isolated session auth mode is not configured",
		)
		return
	}
	if isolatedRunner == nil {
		resp := model.CapabilitiesResponse{
			Available:                 false,
			CommitSupported:           false,
			DiffSupported:             false,
			SessionAuthMode:           string(mode),
			SessionCapabilityRequired: mode.CapabilityRequired(),
		}
		if isolatedProbeResult != nil {
			resp.Isolator = isolatedProbeResult.Isolator
			resp.Version = isolatedProbeResult.Version
			resp.Message = isolatedProbeResult.Message
			resp.SetprivAvailable = isolatedProbeResult.SetprivAvailable
			resp.UsernsAvailable = isolatedProbeResult.UsernsAvailable
		}
		c.RespondSuccess(resp)
		return
	}
	caps := isolatedRunner.Capabilities()
	resp := model.CapabilitiesResponse{
		Available:                 caps.Available,
		Isolator:                  caps.Isolator,
		Version:                   caps.Version,
		SetprivAvailable:          caps.SetprivAvailable,
		UsernsAvailable:           caps.UsernsAvailable,
		CommitSupported:           caps.CommitSupported,
		DiffSupported:             caps.DiffSupported,
		SessionAuthMode:           string(mode),
		SessionCapabilityRequired: mode.CapabilityRequired(),
	}
	// Probe results indicate overlay capability, not diff/commit implementation.
	// Diff and commit are Phase 2; do not advertise them as supported.
	resp.CommitSupported = false
	resp.DiffSupported = false
	c.RespondSuccess(resp)
}

// Filesystem proxy handlers are in isolated_session_files.go.
