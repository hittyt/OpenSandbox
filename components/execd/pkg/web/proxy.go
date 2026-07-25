// Copyright 2025 Alibaba Group Holding Ltd.
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
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/log"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func ProxyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, "/proxy/") {
			c.Next()
			return
		}

		r := c.Request
		w := c.Writer

		rest := strings.TrimPrefix(r.URL.Path, "/proxy/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, "port is required", http.StatusBadRequest)
			c.Abort()
			return
		}

		port, ok := parseProxyPort(parts[0])
		if !ok {
			http.Error(w, "invalid port", http.StatusBadRequest)
			c.Abort()
			return
		}
		path := "/"
		if len(parts) == 2 && parts[1] != "" {
			path += parts[1]
		}

		target := &url.URL{
			Scheme: "http",
			Host:   "127.0.0.1:" + port,
			Path:   path,
		}

		isWebSocket := strings.ToLower(r.Header.Get("Upgrade")) == "websocket"

		proxy := httputil.NewSingleHostReverseProxy(target)
		// Flush SSE chunks promptly; a small interval avoids buffering breaks chunked streams.
		proxy.FlushInterval = 200 * time.Millisecond

		proxy.Director = func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "127.0.0.1:" + port
			req.URL.Path = path
			req.URL.RawQuery = r.URL.RawQuery
			req.URL.RawPath = ""
			req.RequestURI = ""
			stripProxyControlHeaders(req.Header)

			req.Header.Set("X-Forwarded-For", getClientIP(r))
			req.Header.Set("X-Forwarded-Proto", "http")
			req.Header.Del("X-Forwarded-Host")

			if isWebSocket {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Sec-WebSocket-Version", "13")
				if key := r.Header.Get("Sec-WebSocket-Key"); key != "" {
					req.Header.Set("Sec-WebSocket-Key", key)
				}
			}
		}

		proxy.Transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   600 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     600 * time.Second,
		}

		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			log.Error(
				"Proxy upstream request failed: method=%s route=/proxy/:port/*path error_type=%T",
				req.Method,
				err,
			)
			http.Error(rw, "Bad Gateway", http.StatusBadGateway)
		}

		log.Info(
			"Proxy: %s /proxy/:port/*path (WebSocket: %v)",
			r.Method,
			isWebSocket,
		)

		proxy.ServeHTTP(w, r)
		c.Abort()
	}
}

func parseProxyPort(raw string) (string, bool) {
	if raw == "" || strings.Trim(raw, "0123456789") != "" {
		return "", false
	}
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || port == 0 {
		return "", false
	}
	return strconv.FormatUint(port, 10), true
}

func stripProxyControlHeaders(header http.Header) {
	for key := range header {
		lowerKey := strings.ToLower(key)
		if strings.EqualFold(key, model.SessionCapabilityHeader) ||
			strings.EqualFold(key, model.ApiAccessTokenHeader) ||
			strings.HasPrefix(
				lowerKey,
				strings.ToLower(model.ManagerControlHeaderPrefix),
			) ||
			strings.HasPrefix(
				lowerKey,
				strings.ToLower(model.LegacyManagerControlHeaderPrefix),
			) {
			delete(header, key)
		}
	}
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
