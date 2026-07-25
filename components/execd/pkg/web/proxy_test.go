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

package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestProxyMiddlewareReturnsSidecarForbiddenForActiveVault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	receivedPath := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath <- r.URL.Path
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(backendURL.Host)
	require.NoError(t, err)

	router := gin.New()
	router.Use(ProxyMiddleware())
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/proxy/"+port+"/credential-vault/_active", nil)
	require.NoError(t, err)
	resp, err := proxyServer.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "/credential-vault/_active", <-receivedPath)
}

func TestProxyMiddlewareStripsExecdControlCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	receivedHeaders := make(chan http.Header, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		receivedHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(backendURL.Host)
	require.NoError(t, err)

	router := gin.New()
	router.Use(ProxyMiddleware())
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	req, err := http.NewRequest(
		http.MethodGet,
		proxyServer.URL+"/proxy/"+port+"/service?secret=query-value",
		nil,
	)
	require.NoError(t, err)
	req.Header[model.SessionCapabilityHeader] = []string{"capability"}
	req.Header["x-execd-access-token"] = []string{"access-token"}
	req.Header["X-EXECD-Manager-Key-Id"] = []string{"manager-key-id"}
	req.Header["X-EXECD-Manager-Timestamp"] = []string{"manager-timestamp"}
	req.Header["X-EXECD-Manager-Nonce"] = []string{"manager-nonce"}
	req.Header["X-EXECD-Manager-Signature"] = []string{"manager-signature"}
	req.Header["X-OpenSandbox-Manager-Signature"] = []string{"signature"}
	req.Header.Set("X-User-Header", "preserved")

	resp, err := proxyServer.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	headers := <-receivedHeaders
	require.Empty(t, headers.Values(model.SessionCapabilityHeader))
	require.Empty(t, headers.Values(model.ApiAccessTokenHeader))
	require.Empty(t, headers.Values("X-EXECD-Manager-Key-Id"))
	require.Empty(t, headers.Values("X-EXECD-Manager-Timestamp"))
	require.Empty(t, headers.Values("X-EXECD-Manager-Nonce"))
	require.Empty(t, headers.Values("X-EXECD-Manager-Signature"))
	require.Empty(t, headers.Values("X-OpenSandbox-Manager-Signature"))
	require.Equal(t, "preserved", headers.Get("X-User-Header"))
}

func TestParseProxyPort(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "minimum", raw: "1", want: "1", ok: true},
		{name: "maximum", raw: "65535", want: "65535", ok: true},
		{name: "canonicalizes leading zeroes", raw: "080", want: "80", ok: true},
		{name: "zero", raw: "0"},
		{name: "too large", raw: "65536"},
		{name: "negative", raw: "-1"},
		{name: "capability", raw: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "service name", raw: "http"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseProxyPort(test.raw)
			require.Equal(t, test.ok, ok)
			require.Equal(t, test.want, got)
		})
	}
}
