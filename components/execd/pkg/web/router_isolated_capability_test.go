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

//go:build !windows

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	execdlog "github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

const capabilityRouterAccessToken = "capability-router-access-token"

type capabilityRouterTestIsolator struct{}

func (*capabilityRouterTestIsolator) Name() string    { return "test" }
func (*capabilityRouterTestIsolator) Available() bool { return true }
func (*capabilityRouterTestIsolator) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{
		Available:              true,
		Isolator:               "test",
		SetprivAvailable:       true,
		SetprivSwitchAvailable: true,
		UsernsAvailable:        true,
	}
}
func (*capabilityRouterTestIsolator) Wrap(
	_ *exec.Cmd,
	_ isolation.WrapOptions,
) error {
	return nil
}
func (i *capabilityRouterTestIsolator) WrapWithLifecycle(
	cmd *exec.Cmd,
	opts isolation.WrapOptions,
) (isolation.WorkloadLifecycle, error) {
	if err := i.Wrap(cmd, opts); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	close(done)
	return &capabilityRouterTestLifecycle{done: done}, nil
}

type capabilityRouterTestLifecycle struct {
	done chan struct{}
}

func (*capabilityRouterTestLifecycle) WaitForIdentity(
	context.Context,
) (isolation.WorkloadIdentity, error) {
	return isolation.WorkloadIdentity{
		PID:                   2,
		SandboxPID:            1,
		NetNamespaceID:        1,
		ProcessStartTimeTicks: 1,
	}, nil
}
func (*capabilityRouterTestLifecycle) MarkReady() error             { return nil }
func (*capabilityRouterTestLifecycle) Abort()                       {}
func (l *capabilityRouterTestLifecycle) DrainDone() <-chan struct{} { return l.done }
func (*capabilityRouterTestLifecycle) DrainError() error            { return nil }
func (*capabilityRouterTestLifecycle) ExitCode() (int, bool)        { return 0, true }
func (*capabilityRouterTestLifecycle) Close() error                 { return nil }

type isolatedCapabilityRouterFixture struct {
	router http.Handler
	mode   controller.IsolatedSessionAuthMode
}

func setupIsolatedCapabilityRouter(
	t *testing.T,
	mode controller.IsolatedSessionAuthMode,
) *isolatedCapabilityRouterFixture {
	t.Helper()
	runner, err := runtime.NewIsolatedRunner(
		runtime.NewController("", ""),
		&capabilityRouterTestIsolator{},
		isolation.Config{
			UpperRoot:     t.TempDir(),
			UpperMaxBytes: 8 << 30,
		},
	)
	if err != nil {
		t.Fatalf("NewIsolatedRunner: %v", err)
	}
	controller.InitIsolatedRunner(runner)
	t.Cleanup(func() {
		controller.InitIsolatedRunner(nil)
		if err := runner.Close(); err != nil {
			t.Errorf("close isolated runner: %v", err)
		}
	})
	return &isolatedCapabilityRouterFixture{
		router: NewRouterWithIsolatedSessionAuthMode(
			capabilityRouterAccessToken,
			mode,
		),
		mode: mode,
	}
}

func (f *isolatedCapabilityRouterFixture) request(
	method string,
	path string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	f.router.ServeHTTP(recorder, req)
	return recorder
}

func (f *isolatedCapabilityRouterFixture) requestWithCapabilities(
	method string,
	path string,
	body string,
	capabilities ...string,
) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(model.ApiAccessTokenHeader, capabilityRouterAccessToken)
	for _, capability := range capabilities {
		req.Header.Add(model.SessionCapabilityHeader, capability)
	}
	f.router.ServeHTTP(recorder, req)
	return recorder
}

func (f *isolatedCapabilityRouterFixture) requestWithCapabilityHeaderNames(
	method string,
	path string,
	capability string,
	headerNames ...string,
) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(model.ApiAccessTokenHeader, capabilityRouterAccessToken)
	for _, headerName := range headerNames {
		// Assign directly so the test can represent a Header map containing
		// non-canonical case variants in addition to the canonical wire name.
		req.Header[headerName] = append(req.Header[headerName], capability)
	}
	f.router.ServeHTTP(recorder, req)
	return recorder
}

func createIsolatedCapabilitySession(
	t *testing.T,
	fixture *isolatedCapabilityRouterFixture,
) model.IsolatedCreateSessionResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"workspace": map[string]any{
			"path": filepath.Join(t.TempDir(), "workspace"),
			"mode": "rw",
		},
		"share_net": false,
	})
	if err != nil {
		t.Fatalf("encode create request: %v", err)
	}
	recorder := fixture.request(
		http.MethodPost,
		"/v1/isolated/session",
		string(body),
		map[string]string{
			model.ApiAccessTokenHeader: capabilityRouterAccessToken,
			"Content-Type":             "application/json",
		},
	)
	if recorder.Code != http.StatusCreated {
		t.Fatalf(
			"create status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusCreated,
			recorder.Body.String(),
		)
	}
	var response model.IsolatedCreateSessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if response.SessionID == "" {
		t.Fatal("create response has empty session_id")
	}
	if fixture.mode.CapabilityRequired() {
		if response.Capability == "" {
			t.Fatal("capability-mode create response has empty capability")
		}
		if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("create Cache-Control = %q, want %q", cacheControl, "no-store")
		}
	} else if response.Capability != "" {
		t.Fatalf(
			"legacy create response leaked capability %q",
			response.Capability,
		)
	}
	return response
}

func capabilityHeaders(capability string) map[string]string {
	return map[string]string{
		model.ApiAccessTokenHeader:    capabilityRouterAccessToken,
		model.SessionCapabilityHeader: capability,
	}
}

func endCapabilityRouterTestShell(
	t *testing.T,
	fixture *isolatedCapabilityRouterFixture,
	base string,
	headers map[string]string,
) {
	t.Helper()
	requestHeaders := make(map[string]string, len(headers)+1)
	for key, value := range headers {
		requestHeaders[key] = value
	}
	requestHeaders["Content-Type"] = "application/json"
	recorder := fixture.request(
		http.MethodPost,
		base+"/run",
		`{"code":"exit"}`,
		requestHeaders,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"end test shell status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusOK,
			recorder.Body.String(),
		)
	}
}

func assertCapabilityError(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf(
			"status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusForbidden,
			recorder.Body.String(),
		)
	}
	var response model.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode capability error: %v", err)
	}
	if response.Code != model.ErrorCodeSessionCapabilityInvalid {
		t.Fatalf(
			"error code = %q, want %q",
			response.Code,
			model.ErrorCodeSessionCapabilityInvalid,
		)
	}
}

func assertRouteResponse(
	t *testing.T,
	recorder *httptest.ResponseRecorder,
	wantStatus int,
	wantCode model.ErrorCode,
) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf(
			"status = %d, want %d; body=%s",
			recorder.Code,
			wantStatus,
			recorder.Body.String(),
		)
	}
	if wantCode == "" {
		return
	}
	var response model.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode handler error: %v", err)
	}
	if response.Code != wantCode {
		t.Fatalf("code = %q, want %q", response.Code, wantCode)
	}
}

func TestIsolatedSessionAuthModesAllowAuthorizedRoutes(t *testing.T) {
	modes := []struct {
		name              string
		mode              controller.IsolatedSessionAuthMode
		includeCapability bool
	}{
		{
			name: "legacy without capability header",
			mode: controller.IsolatedSessionAuthModeLegacy,
		},
		{
			name:              "capability with matching header",
			mode:              controller.IsolatedSessionAuthModeCapability,
			includeCapability: true,
		},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			fixture := setupIsolatedCapabilityRouter(t, mode.mode)
			created := createIsolatedCapabilitySession(t, fixture)
			base := "/v1/isolated/session/" + created.SessionID
			headers := map[string]string{
				model.ApiAccessTokenHeader: capabilityRouterAccessToken,
			}
			if mode.includeCapability {
				headers[model.SessionCapabilityHeader] = created.Capability
			}

			routes := []struct {
				name       string
				method     string
				suffix     string
				body       string
				wantStatus int
				wantCode   model.ErrorCode
			}{
				{
					name:       "get",
					method:     http.MethodGet,
					wantStatus: http.StatusOK,
				},
				{
					name:       "run",
					method:     http.MethodPost,
					suffix:     "/run",
					body:       `{"code":"printf authorized-ok"}`,
					wantStatus: http.StatusOK,
				},
				{
					name:       "files",
					method:     http.MethodGet,
					suffix:     "/files/info",
					wantStatus: http.StatusOK,
				},
				{
					name:       "diff reaches handler",
					method:     http.MethodGet,
					suffix:     "/diff",
					wantStatus: http.StatusServiceUnavailable,
					wantCode:   model.ErrorCodeNotSupported,
				},
				{
					name:       "commit reaches handler",
					method:     http.MethodPost,
					suffix:     "/commit",
					wantStatus: http.StatusServiceUnavailable,
					wantCode:   model.ErrorCodeNotSupported,
				},
			}
			for _, route := range routes {
				t.Run(route.name, func(t *testing.T) {
					requestHeaders := make(map[string]string, len(headers)+1)
					for key, value := range headers {
						requestHeaders[key] = value
					}
					if route.body != "" {
						requestHeaders["Content-Type"] = "application/json"
					}
					assertRouteResponse(
						t,
						fixture.request(
							route.method,
							base+route.suffix,
							route.body,
							requestHeaders,
						),
						route.wantStatus,
						route.wantCode,
					)
				})
			}

			endCapabilityRouterTestShell(t, fixture, base, headers)
			assertRouteResponse(
				t,
				fixture.request(http.MethodDelete, base, "", headers),
				http.StatusOK,
				"",
			)
			if mode.includeCapability {
				assertCapabilityError(
					t,
					fixture.request(http.MethodGet, base, "", headers),
				)
			}
		})
	}
}

func TestCapabilityModeGuardsEverySessionScopedRoute(t *testing.T) {
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeCapability,
	)
	created := createIsolatedCapabilitySession(t, fixture)
	base := "/v1/isolated/session/" + created.SessionID

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, base},
		{http.MethodPost, base + "/run"},
		{http.MethodDelete, base},
		{http.MethodGet, base + "/diff"},
		{http.MethodPost, base + "/commit"},
		{http.MethodGet, base + "/files/info"},
		{http.MethodGet, base + "/files/download"},
		{http.MethodPost, base + "/files/upload"},
		{http.MethodDelete, base + "/files"},
		{http.MethodPost, base + "/files/mv"},
		{http.MethodPost, base + "/files/permissions"},
		{http.MethodPost, base + "/files/replace"},
		{http.MethodGet, base + "/files/search"},
		{http.MethodGet, base + "/directories/list"},
		{http.MethodPost, base + "/directories"},
		{http.MethodDelete, base + "/directories"},
	}
	for _, route := range routes {
		name := route.method + " " + route.path
		t.Run(name, func(t *testing.T) {
			for _, capability := range []string{"", "wrong-capability"} {
				var recorder *httptest.ResponseRecorder
				if capability == "" {
					recorder = fixture.requestWithCapabilities(
						route.method,
						route.path,
						"",
					)
				} else {
					recorder = fixture.requestWithCapabilities(
						route.method,
						route.path,
						"",
						capability,
					)
				}
				assertCapabilityError(t, recorder)
			}
		})
	}
}

func TestCapabilityModeUsesOneUniformErrorForInvalidCredentials(t *testing.T) {
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeCapability,
	)
	first := createIsolatedCapabilitySession(t, fixture)
	second := createIsolatedCapabilitySession(t, fixture)
	path := "/v1/isolated/session/" + first.SessionID

	recorders := map[string]*httptest.ResponseRecorder{
		"missing": fixture.requestWithCapabilities(
			http.MethodGet,
			path,
			"",
		),
		"wrong": fixture.requestWithCapabilities(
			http.MethodGet,
			path,
			"",
			"wrong-capability",
		),
		"cross session": fixture.requestWithCapabilities(
			http.MethodGet,
			path,
			"",
			second.Capability,
		),
		"duplicate header": fixture.requestWithCapabilities(
			http.MethodGet,
			path,
			"",
			first.Capability,
			first.Capability,
		),
	}

	var canonicalBody string
	for name, recorder := range recorders {
		t.Run(name, func(t *testing.T) {
			assertCapabilityError(t, recorder)
			if canonicalBody == "" {
				canonicalBody = recorder.Body.String()
				return
			}
			if recorder.Body.String() != canonicalBody {
				t.Fatalf(
					"body = %q, want uniform response %q",
					recorder.Body.String(),
					canonicalBody,
				)
			}
		})
	}
}

func TestCapabilityModeDeleteRejectsDuplicateCapabilityHeaderSpellings(
	t *testing.T,
) {
	tests := []struct {
		name        string
		headerNames func() []string
	}{
		{
			name: "duplicate values",
			headerNames: func() []string {
				canonical := http.CanonicalHeaderKey(
					model.SessionCapabilityHeader,
				)
				return []string{canonical, canonical}
			},
		},
		{
			name: "case variant duplicate names",
			headerNames: func() []string {
				return []string{
					http.CanonicalHeaderKey(model.SessionCapabilityHeader),
					strings.ToLower(model.SessionCapabilityHeader),
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupIsolatedCapabilityRouter(
				t,
				controller.IsolatedSessionAuthModeCapability,
			)
			created := createIsolatedCapabilitySession(t, fixture)
			path := "/v1/isolated/session/" + created.SessionID
			recorder := fixture.requestWithCapabilityHeaderNames(
				http.MethodDelete,
				path,
				created.Capability,
				test.headerNames()...,
			)
			assertCapabilityError(t, recorder)

			// Rejected duplicate credentials must not revoke the valid
			// capability or start deletion.
			assertRouteResponse(
				t,
				fixture.request(
					http.MethodGet,
					path,
					"",
					capabilityHeaders(created.Capability),
				),
				http.StatusOK,
				"",
			)
		})
	}
}

func TestCapabilityModeForbidsSessionList(t *testing.T) {
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeCapability,
	)
	_ = createIsolatedCapabilitySession(t, fixture)
	recorder := fixture.request(
		http.MethodGet,
		"/v1/isolated/sessions",
		"",
		map[string]string{
			model.ApiAccessTokenHeader: capabilityRouterAccessToken,
		},
	)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf(
			"list status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusForbidden,
			recorder.Body.String(),
		)
	}
	var response model.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode list error: %v", err)
	}
	if response.Code != model.ErrorCodeSessionListForbidden {
		t.Fatalf(
			"list code = %q, want %q",
			response.Code,
			model.ErrorCodeSessionListForbidden,
		)
	}
}

func TestCapabilitiesReportIsolatedSessionAuthMode(t *testing.T) {
	tests := []struct {
		name               string
		mode               controller.IsolatedSessionAuthMode
		capabilityRequired bool
	}{
		{
			name: "legacy",
			mode: controller.IsolatedSessionAuthModeLegacy,
		},
		{
			name:               "capability",
			mode:               controller.IsolatedSessionAuthModeCapability,
			capabilityRequired: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupIsolatedCapabilityRouter(t, test.mode)
			recorder := fixture.request(
				http.MethodGet,
				"/v1/isolated/capabilities",
				"",
				map[string]string{
					model.ApiAccessTokenHeader: capabilityRouterAccessToken,
				},
			)
			assertRouteResponse(t, recorder, http.StatusOK, "")

			var response model.CapabilitiesResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode capabilities response: %v", err)
			}
			if response.SessionAuthMode != string(test.mode) {
				t.Fatalf(
					"session_auth_mode = %q, want %q",
					response.SessionAuthMode,
					test.mode,
				)
			}
			if response.SessionCapabilityRequired != test.capabilityRequired {
				t.Fatalf(
					"session_capability_required = %v, want %v",
					response.SessionCapabilityRequired,
					test.capabilityRequired,
				)
			}
		})
	}
}

func TestCapabilityModeFailsClosedWithoutPrivateNetworkBackend(t *testing.T) {
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeCapability,
	)
	tests := []struct {
		name       string
		shareNet   any
		wantStatus int
		wantCode   model.ErrorCode
	}{
		{
			name:       "omitted requires hardened default backend",
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   model.ErrorCodeSessionNetworkBackendUnavailable,
		},
		{
			name:       "shared parent network is forbidden",
			shareNet:   true,
			wantStatus: http.StatusBadRequest,
			wantCode:   model.ErrorCodeSessionSharedNetworkForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			requestBody := map[string]any{
				"workspace": map[string]any{
					"path": workspace,
					"mode": "rw",
				},
			}
			if test.shareNet != nil {
				requestBody["share_net"] = test.shareNet
			}
			body, err := json.Marshal(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			recorder := fixture.request(
				http.MethodPost,
				"/v1/isolated/session",
				string(body),
				map[string]string{
					model.ApiAccessTokenHeader: capabilityRouterAccessToken,
					"Content-Type":             "application/json",
				},
			)
			if recorder.Code != test.wantStatus {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					recorder.Code,
					test.wantStatus,
					recorder.Body.String(),
				)
			}
			var response model.ErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode create error: %v", err)
			}
			if response.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", response.Code, test.wantCode)
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf(
					"rejected secure create touched workspace: %v",
					err,
				)
			}
		})
	}
}

func TestLegacyModePreservesOmittedShareNetCompatibility(t *testing.T) {
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeLegacy,
	)
	body, err := json.Marshal(map[string]any{
		"workspace": map[string]any{
			"path": filepath.Join(t.TempDir(), "workspace"),
			"mode": "rw",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{
		model.ApiAccessTokenHeader: capabilityRouterAccessToken,
		"Content-Type":             "application/json",
	}
	recorder := fixture.request(
		http.MethodPost,
		"/v1/isolated/session",
		string(body),
		headers,
	)
	if recorder.Code != http.StatusCreated {
		t.Fatalf(
			"legacy create status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusCreated,
			recorder.Body.String(),
		)
	}
	var created model.IsolatedCreateSessionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Capability != "" {
		t.Fatal("legacy create exposed a capability")
	}
	base := "/v1/isolated/session/" + created.SessionID
	endCapabilityRouterTestShell(t, fixture, base, headers)
	deleted := fixture.request(http.MethodDelete, base, "", headers)
	if deleted.Code != http.StatusOK {
		t.Fatalf(
			"legacy delete status = %d, want %d; body=%s",
			deleted.Code,
			http.StatusOK,
			deleted.Body.String(),
		)
	}
}

func TestRawCapabilityAppearsOnlyInCreateResponse(t *testing.T) {
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeCapability,
	)
	created := createIsolatedCapabilitySession(t, fixture)
	responses := map[string]*httptest.ResponseRecorder{
		"state": fixture.request(
			http.MethodGet,
			"/v1/isolated/session/"+created.SessionID,
			"",
			map[string]string{
				model.ApiAccessTokenHeader:    capabilityRouterAccessToken,
				model.SessionCapabilityHeader: created.Capability,
			},
		),
		"list": fixture.request(
			http.MethodGet,
			"/v1/isolated/sessions",
			"",
			map[string]string{
				model.ApiAccessTokenHeader: capabilityRouterAccessToken,
			},
		),
		"capabilities": fixture.request(
			http.MethodGet,
			"/v1/isolated/capabilities",
			"",
			map[string]string{
				model.ApiAccessTokenHeader: capabilityRouterAccessToken,
			},
		),
	}
	for name, recorder := range responses {
		t.Run(name, func(t *testing.T) {
			wantStatus := http.StatusOK
			if name == "list" {
				wantStatus = http.StatusForbidden
			}
			if recorder.Code != wantStatus {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					recorder.Code,
					wantStatus,
					recorder.Body.String(),
				)
			}
			if bytes.Contains(recorder.Body.Bytes(), []byte(created.Capability)) {
				t.Fatalf("response leaked raw capability: %s", recorder.Body.String())
			}
		})
	}
}

func setupCapabilityRouterLogCapture(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "execd.log")
	previousLogPath, hadPreviousLogPath := os.LookupEnv("EXECD_LOG_FILE")
	if err := os.Setenv("EXECD_LOG_FILE", logPath); err != nil {
		t.Fatalf("set EXECD_LOG_FILE: %v", err)
	}
	execdlog.Init(5)
	t.Cleanup(func() {
		if hadPreviousLogPath {
			_ = os.Setenv("EXECD_LOG_FILE", previousLogPath)
		} else {
			_ = os.Unsetenv("EXECD_LOG_FILE")
		}
		execdlog.Init(5)
	})
	return logPath
}

func readCapabilityRouterLog(t *testing.T, logPath string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		content, err := os.ReadFile(logPath)
		if err == nil && len(content) > 0 {
			return content
		}
		if time.Now().After(deadline) {
			t.Fatalf("read request log: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertLogOmitsSecrets(t *testing.T, content []byte, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("request log leaked secret %q: %s", secret, content)
		}
	}
}

func TestRequestLoggingDoesNotLeakRawURLSecrets(t *testing.T) {
	logPath := setupCapabilityRouterLogCapture(t)
	fixture := setupIsolatedCapabilityRouter(
		t,
		controller.IsolatedSessionAuthModeCapability,
	)
	created := createIsolatedCapabilitySession(t, fixture)
	rawURLSecret := "raw url/secret?" + created.Capability
	path := "/v1/isolated/session/" + created.SessionID +
		"?capability=" + url.QueryEscape(created.Capability) +
		"&token=" + url.QueryEscape(rawURLSecret)
	recorder := fixture.request(
		http.MethodGet,
		path,
		"",
		map[string]string{
			model.ApiAccessTokenHeader: capabilityRouterAccessToken,
		},
	)
	assertCapabilityError(t, recorder)

	assertLogOmitsSecrets(
		t,
		readCapabilityRouterLog(t, logPath),
		created.Capability,
		rawURLSecret,
		url.QueryEscape(rawURLSecret),
	)
}

func TestSafeRecoveryDoesNotLeakRequestSecrets(t *testing.T) {
	logPath := setupCapabilityRouterLogCapture(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(safeRecoveryMiddleware(), logMiddleware())
	router.GET("/panic/:value", func(*gin.Context) {
		panic("intentional test panic")
	})

	capabilitySecret := "capability-secret-only-in-header"
	accessTokenSecret := "access-token-secret-only-in-header"
	pathSecret := "secret-path-value"
	querySecret := "secret-query-value"
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/panic/"+pathSecret+"?token="+url.QueryEscape(querySecret),
		nil,
	)
	req.Header.Set(model.SessionCapabilityHeader, capabilitySecret)
	req.Header.Set(model.ApiAccessTokenHeader, accessTokenSecret)
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf(
			"panic status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusInternalServerError,
			recorder.Body.String(),
		)
	}
	content := readCapabilityRouterLog(t, logPath)
	if !bytes.Contains(content, []byte("/panic/:value")) {
		t.Fatalf("recovery did not log the safe route template: %s", content)
	}
	assertLogOmitsSecrets(
		t,
		content,
		capabilitySecret,
		accessTokenSecret,
		pathSecret,
		querySecret,
	)
}

func TestProxyRejectsCapabilityShapedPortWithoutLoggingIt(t *testing.T) {
	logPath := setupCapabilityRouterLogCapture(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ProxyMiddleware())

	capabilitySecret := strings.Repeat("A", 43)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/proxy/"+capabilitySecret+"/service",
		nil,
	)
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf(
			"invalid proxy port status = %d, want %d; body=%s",
			recorder.Code,
			http.StatusBadRequest,
			recorder.Body.String(),
		)
	}

	// Emit a sentinel so the asynchronous file logger has content to flush
	// even though invalid ports are rejected before proxy logging.
	execdlog.Info("proxy invalid port regression sentinel")
	assertLogOmitsSecrets(
		t,
		readCapabilityRouterLog(t, logPath),
		capabilitySecret,
	)
}
