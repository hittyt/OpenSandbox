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
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

type shutdownRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *shutdownRecorder) add(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *shutdownRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

type fakeLifecycleServer struct {
	recorder    *shutdownRecorder
	serveErr    error
	shutdownErr error
	started     chan struct{}
	stopped     chan struct{}
	startOnce   sync.Once
	stopOnce    sync.Once
}

func newBlockingLifecycleServer(recorder *shutdownRecorder) *fakeLifecycleServer {
	return &fakeLifecycleServer{
		recorder: recorder,
		started:  make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

func (s *fakeLifecycleServer) Serve(net.Listener) error {
	s.startOnce.Do(func() { close(s.started) })
	if s.serveErr != nil {
		return s.serveErr
	}
	<-s.stopped
	return nil
}

func (s *fakeLifecycleServer) Shutdown(context.Context) error {
	s.recorder.add("http")
	s.stopOnce.Do(func() { close(s.stopped) })
	return s.shutdownErr
}

type fakeLifecycleCloser struct {
	recorder *shutdownRecorder
	err      error
}

func (c *fakeLifecycleCloser) Close() error {
	c.recorder.add("runner")
	return c.err
}

func TestProcessLifecycleShutdownOrderAndErrorAggregation(t *testing.T) {
	recorder := &shutdownRecorder{}
	httpErr := errors.New("http failed")
	runnerErr := errors.New("runner failed")
	telemetryErr := errors.New("telemetry failed")
	server := newBlockingLifecycleServer(recorder)
	server.shutdownErr = httpErr
	runner := &fakeLifecycleCloser{recorder: recorder, err: runnerErr}

	lifecycle := &processLifecycle{
		server: server,
		runner: runner,
		telemetryShutdown: func(context.Context) error {
			recorder.add("telemetry")
			return telemetryErr
		},
		httpTimeout:      time.Second,
		telemetryTimeout: time.Second,
	}

	err := lifecycle.shutdown()
	for _, want := range []error{httpErr, runnerErr, telemetryErr} {
		if !errors.Is(err, want) {
			t.Fatalf("shutdown error %v does not contain %v", err, want)
		}
	}
	if got, want := recorder.snapshot(), []string{"http", "runner", "telemetry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestProcessLifecycleShutdownIsConcurrentAndIdempotent(t *testing.T) {
	recorder := &shutdownRecorder{}
	server := newBlockingLifecycleServer(recorder)
	runner := &fakeLifecycleCloser{recorder: recorder}
	lifecycle := &processLifecycle{
		server: server,
		runner: runner,
		telemetryShutdown: func(context.Context) error {
			recorder.add("telemetry")
			return nil
		},
		httpTimeout:      time.Second,
		telemetryTimeout: time.Second,
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lifecycle.shutdown(); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		}()
	}
	wg.Wait()

	if got, want := recorder.snapshot(), []string{"http", "runner", "telemetry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown calls = %v, want exactly %v", got, want)
	}
}

func TestServeUntilShutdownStopsAdmissionBeforeRunner(t *testing.T) {
	recorder := &shutdownRecorder{}
	server := newBlockingLifecycleServer(recorder)
	runner := &fakeLifecycleCloser{recorder: recorder}
	lifecycle := &processLifecycle{
		server: server,
		runner: runner,
		telemetryShutdown: func(context.Context) error {
			recorder.add("telemetry")
			return nil
		},
		httpTimeout:      time.Second,
		telemetryTimeout: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		result <- serveUntilShutdown(ctx, server, nil, lifecycle)
	}()

	select {
	case <-server.started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serveUntilShutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveUntilShutdown did not return after cancellation")
	}

	if got, want := recorder.snapshot(), []string{"http", "runner", "telemetry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestServeUntilShutdownCleansUpAfterUnexpectedServerExit(t *testing.T) {
	recorder := &shutdownRecorder{}
	serveErr := errors.New("serve failed")
	server := newBlockingLifecycleServer(recorder)
	server.serveErr = serveErr
	runner := &fakeLifecycleCloser{recorder: recorder}
	lifecycle := &processLifecycle{
		server:           server,
		runner:           runner,
		httpTimeout:      time.Second,
		telemetryTimeout: time.Second,
	}

	err := serveUntilShutdown(context.Background(), server, nil, lifecycle)
	if !errors.Is(err, serveErr) {
		t.Fatalf("serveUntilShutdown error = %v, want %v", err, serveErr)
	}
	if got, want := recorder.snapshot(), []string{"http", "runner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestProcessLifecycleWithoutIsolatedRunner(t *testing.T) {
	recorder := &shutdownRecorder{}
	server := newBlockingLifecycleServer(recorder)
	lifecycle := &processLifecycle{
		server: server,
		telemetryShutdown: func(context.Context) error {
			recorder.add("telemetry")
			return nil
		},
		httpTimeout:      time.Second,
		telemetryTimeout: time.Second,
	}

	if err := lifecycle.shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got, want := recorder.snapshot(), []string{"http", "telemetry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordinary sandbox shutdown = %v, want %v", got, want)
	}
}
