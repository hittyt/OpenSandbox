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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	defaultHTTPShutdownTimeout      = 10 * time.Second
	defaultTelemetryShutdownTimeout = 5 * time.Second
)

type lifecycleHTTPServer interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
}

type lifecycleCloser interface {
	Close() error
}

type telemetryShutdownFunc func(context.Context) error

// processLifecycle owns the process-level shutdown order. HTTP is stopped
// first so no new requests can enter isolated-session admission while the
// runner is being drained and closed. Telemetry is stopped last so cleanup
// remains observable.
//
// It intentionally does not attempt to recover an isolated runner or its
// sessions. Execd shutdown terminates those process-owned resources.
type processLifecycle struct {
	server            lifecycleHTTPServer
	runner            lifecycleCloser
	telemetryShutdown telemetryShutdownFunc

	httpTimeout      time.Duration
	telemetryTimeout time.Duration

	once sync.Once
	err  error
}

func (l *processLifecycle) shutdown() error {
	l.once.Do(func() {
		var shutdownErrs []error

		if l.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), l.httpTimeout)
			err := l.server.Shutdown(ctx)
			cancel()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				shutdownErrs = append(shutdownErrs, fmt.Errorf("http server shutdown: %w", err))
			}
		}

		if l.runner != nil {
			if err := l.runner.Close(); err != nil {
				shutdownErrs = append(shutdownErrs, fmt.Errorf("isolated runner shutdown: %w", err))
			}
		}

		if l.telemetryShutdown != nil {
			ctx, cancel := context.WithTimeout(context.Background(), l.telemetryTimeout)
			err := l.telemetryShutdown(ctx)
			cancel()
			if err != nil {
				shutdownErrs = append(shutdownErrs, fmt.Errorf("telemetry shutdown: %w", err))
			}
		}

		l.err = errors.Join(shutdownErrs...)
	})
	return l.err
}

// serveUntilShutdown runs the HTTP server until the process context is
// cancelled or the server exits. Both paths close all process-owned resources;
// cancellation is a normal exit, while an unexpected server failure is
// returned after cleanup.
func serveUntilShutdown(
	ctx context.Context,
	server lifecycleHTTPServer,
	listener net.Listener,
	lifecycle *processLifecycle,
) error {
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		return lifecycle.shutdown()
	case err := <-serveErrCh:
		var serveErr error
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("execd HTTP server: %w", err)
		}
		return errors.Join(serveErr, lifecycle.shutdown())
	}
}
