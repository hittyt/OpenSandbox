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

package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

type lifecycleHarness struct {
	mu sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
	waitErr   error
	readyErr  error
	drainErr  error
	closeErr  error
	waited    bool
	ready     bool
	aborted   bool
	closed    bool
}

func newLifecycleHarness() *lifecycleHarness {
	return &lifecycleHarness{done: make(chan struct{})}
}

func (l *lifecycleHarness) WaitForIdentity(context.Context) (isolation.WorkloadIdentity, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waited = true
	if l.waitErr != nil {
		return isolation.WorkloadIdentity{}, l.waitErr
	}
	return isolation.WorkloadIdentity{
		PID:                   2,
		SandboxPID:            1,
		NetNamespaceID:        1,
		ProcessStartTimeTicks: 1,
	}, nil
}

func (l *lifecycleHarness) MarkReady() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.readyErr != nil {
		return l.readyErr
	}
	l.ready = true
	return nil
}

func (l *lifecycleHarness) Abort() {
	l.mu.Lock()
	l.aborted = true
	l.mu.Unlock()
	l.finish(nil)
}

func (l *lifecycleHarness) DrainDone() <-chan struct{} {
	return l.done
}

func (l *lifecycleHarness) DrainError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drainErr
}

func (*lifecycleHarness) ExitCode() (int, bool) {
	return 0, false
}

func (l *lifecycleHarness) Close() error {
	l.Abort()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return errors.Join(l.drainErr, l.closeErr)
}

func (l *lifecycleHarness) finish(err error) {
	l.mu.Lock()
	if l.drainErr == nil {
		l.drainErr = err
	}
	l.mu.Unlock()
	l.closeOnce.Do(func() {
		close(l.done)
	})
}

type lifecycleHarnessIsolator struct {
	lifecycle       isolation.WorkloadLifecycle
	wrapErr         error
	returnNil       bool
	attachExtra     bool
	configure       func(*exec.Cmd)
	cmd             *exec.Cmd
	lastOpts        isolation.WrapOptions
	parentExtraFile *os.File
}

func (*lifecycleHarnessIsolator) Name() string {
	return "lifecycle-harness"
}

func (*lifecycleHarnessIsolator) Available() bool {
	return true
}

func (*lifecycleHarnessIsolator) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{Available: true}
}

func (i *lifecycleHarnessIsolator) Wrap(cmd *exec.Cmd, opts isolation.WrapOptions) error {
	i.cmd = cmd
	i.lastOpts = opts
	return nil
}

func (i *lifecycleHarnessIsolator) WrapWithLifecycle(
	cmd *exec.Cmd,
	opts isolation.WrapOptions,
) (isolation.WorkloadLifecycle, error) {
	if err := i.Wrap(cmd, opts); err != nil {
		return nil, err
	}
	if i.attachExtra {
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		_ = writer.Close()
		i.parentExtraFile = reader
		cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
	}
	if i.configure != nil {
		i.configure(cmd)
	}
	if i.returnNil {
		return nil, i.wrapErr
	}
	return i.lifecycle, i.wrapErr
}

type legacyRuntimeIsolator struct {
	wrapCalls int
}

func (*legacyRuntimeIsolator) Name() string {
	return "legacy-runtime"
}

func (*legacyRuntimeIsolator) Available() bool {
	return true
}

func (*legacyRuntimeIsolator) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{Available: true}
}

func (i *legacyRuntimeIsolator) Wrap(*exec.Cmd, isolation.WrapOptions) error {
	i.wrapCalls++
	return nil
}

func newLifecycleRuntimeSession(
	t *testing.T,
	isolator isolation.Isolator,
	shareNet *bool,
) *isolatedSession {
	t.Helper()
	return newIsolatedSession(
		"lifecycle-test",
		&IsolatedSessionOptions{
			WorkspacePath: t.TempDir(),
			WorkspaceMode: string(isolation.WorkspaceRW),
			ShareNet:      shareNet,
		},
		isolator,
	)
}

func assertHarnessCleaned(t *testing.T, lifecycle *lifecycleHarness) {
	t.Helper()
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if !lifecycle.aborted || !lifecycle.closed {
		t.Fatalf(
			"lifecycle cleanup = aborted:%v closed:%v",
			lifecycle.aborted,
			lifecycle.closed,
		)
	}
}

func assertExtraFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	if file == nil {
		t.Fatal("test isolator did not attach an ExtraFiles descriptor")
	}
	if _, err := file.Stat(); err == nil {
		t.Fatal("session startup leaked an ExtraFiles descriptor")
	}
}

func useDirectProcessKill(t *testing.T) {
	t.Helper()
	originalKill := killSessionProcessGroup
	killSessionProcessGroup = func(pid int) error {
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return process.Kill()
	}
	t.Cleanup(func() {
		killSessionProcessGroup = originalKill
	})
}

func TestIsolatedSessionLifecycleIsRequired(t *testing.T) {
	isolator := &legacyRuntimeIsolator{}
	session := newLifecycleRuntimeSession(t, isolator, nil)

	err := session.start()
	if !errors.Is(err, ErrSessionLifecycleUnavailable) {
		t.Fatalf("start error = %v, want %v", err, ErrSessionLifecycleUnavailable)
	}
	if isolator.wrapCalls != 0 {
		t.Fatalf("legacy Wrap called %d times", isolator.wrapCalls)
	}
}

func TestIsolatedSessionRejectsNilLifecycleAndClosesDescriptors(t *testing.T) {
	isolator := &lifecycleHarnessIsolator{
		returnNil:   true,
		attachExtra: true,
	}
	session := newLifecycleRuntimeSession(t, isolator, nil)

	err := session.start()
	if !errors.Is(err, ErrSessionLifecycleUnavailable) {
		t.Fatalf("start error = %v, want %v", err, ErrSessionLifecycleUnavailable)
	}
	assertExtraFileClosed(t, isolator.parentExtraFile)
}

func TestIsolatedSessionPreservesShareNetSemantics(t *testing.T) {
	useDirectProcessKill(t)

	falseValue := false
	trueValue := true
	tests := []struct {
		name     string
		shareNet *bool
		want     bool
	}{
		{name: "omitted keeps legacy default", want: true},
		{name: "false remains false", shareNet: &falseValue, want: false},
		{name: "true remains true", shareNet: &trueValue, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := newLifecycleHarness()
			isolator := &lifecycleHarnessIsolator{lifecycle: lifecycle}
			session := newLifecycleRuntimeSession(t, isolator, tt.shareNet)

			if err := session.start(); err != nil {
				t.Fatal(err)
			}
			if got := isolator.lastOpts.ShareNet; got != tt.want {
				t.Fatalf("WrapOptions.ShareNet = %v, want %v", got, tt.want)
			}
			if err := session.stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIsolatedSessionStartupGateFailureReapsProcess(t *testing.T) {
	waitErr := errors.New("identity unavailable")
	readyErr := errors.New("ready gate failed")
	tests := []struct {
		name     string
		waitErr  error
		readyErr error
	}{
		{name: "identity", waitErr: waitErr},
		{name: "mark ready", readyErr: readyErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := newLifecycleHarness()
			lifecycle.waitErr = tt.waitErr
			lifecycle.readyErr = tt.readyErr
			isolator := &lifecycleHarnessIsolator{
				lifecycle:   lifecycle,
				attachExtra: true,
			}
			session := newLifecycleRuntimeSession(t, isolator, nil)

			err := session.start()
			if !errors.Is(err, tt.waitErr) && !errors.Is(err, tt.readyErr) {
				t.Fatalf("start error = %v", err)
			}
			assertHarnessCleaned(t, lifecycle)
			assertExtraFileClosed(t, isolator.parentExtraFile)
			if isolator.cmd == nil || isolator.cmd.Process == nil {
				t.Fatal("test workload was not started")
			}
			if isolator.cmd.ProcessState == nil {
				t.Fatal("failed startup returned before reaping its workload")
			}
			if session.stdin != nil || session.stdout != nil {
				t.Fatal("failed startup retained command pipes")
			}

			lifecycle.mu.Lock()
			waited := lifecycle.waited
			ready := lifecycle.ready
			lifecycle.mu.Unlock()
			if !waited {
				t.Fatal("WaitForIdentity was not called")
			}
			if tt.waitErr != nil && ready {
				t.Fatal("workload was marked ready after identity failure")
			}
		})
	}
}

func TestIsolatedSessionPreStartFailuresCleanLifecycleAndDescriptors(t *testing.T) {
	wrapErr := errors.New("wrap failed")
	tests := []struct {
		name      string
		wrapErr   error
		configure func(*exec.Cmd)
	}{
		{name: "wrap", wrapErr: wrapErr},
		{
			name: "stdin pipe",
			configure: func(cmd *exec.Cmd) {
				cmd.Stdin = strings.NewReader("")
			},
		},
		{
			name: "stdout pipe",
			configure: func(cmd *exec.Cmd) {
				cmd.Stdout = io.Discard
			},
		},
		{
			name: "command start",
			configure: func(cmd *exec.Cmd) {
				cmd.Path = "/opensandbox-test-command-does-not-exist"
				cmd.Args[0] = cmd.Path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := newLifecycleHarness()
			isolator := &lifecycleHarnessIsolator{
				lifecycle:   lifecycle,
				wrapErr:     tt.wrapErr,
				attachExtra: true,
				configure:   tt.configure,
			}
			session := newLifecycleRuntimeSession(t, isolator, nil)

			if err := session.start(); err == nil {
				t.Fatal("start unexpectedly succeeded")
			}
			assertHarnessCleaned(t, lifecycle)
			assertExtraFileClosed(t, isolator.parentExtraFile)
		})
	}
}

func TestIsolatedSessionFatalLifecycleDrainKillsWorkload(t *testing.T) {
	lifecycle := newLifecycleHarness()
	isolator := &lifecycleHarnessIsolator{lifecycle: lifecycle}
	session := newLifecycleRuntimeSession(t, isolator, nil)
	if err := session.start(); err != nil {
		t.Fatal(err)
	}

	drainErr := errors.New("malformed bwrap status")
	lifecycle.finish(drainErr)
	select {
	case <-session.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("workload survived fatal lifecycle drain failure")
	}
	if !session.dead() {
		t.Fatal("fatal lifecycle drain did not mark the session dead")
	}
	if err := session.stop(); !errors.Is(err, drainErr) {
		t.Fatalf("stop error = %v, want %v", err, drainErr)
	}
}

func TestIsolatedSessionStopIsBounded(t *testing.T) {
	originalKill := killSessionProcessGroup
	originalTimeout := isolatedSessionStopTimeout
	t.Cleanup(func() {
		killSessionProcessGroup = originalKill
		isolatedSessionStopTimeout = originalTimeout
	})

	killErr := syscall.EPERM
	killSessionProcessGroup = func(int) error { return killErr }
	isolatedSessionStopTimeout = 20 * time.Millisecond
	session := &isolatedSession{
		cmd:    &exec.Cmd{Process: &os.Process{Pid: 424242}},
		doneCh: make(chan struct{}),
	}

	started := time.Now()
	err := session.stop()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stop remained unbounded for %v", elapsed)
	}
	if !errors.Is(err, killErr) {
		t.Fatalf("stop error = %v, want %v", err, killErr)
	}
	if !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("stop error = %v, want %v", err, ErrSessionTeardownTimeout)
	}
}

func TestIsolatedSessionStopNeverSignalsAfterProcessWait(t *testing.T) {
	originalKill := killSessionProcessGroup
	originalTimeout := isolatedSessionStopTimeout
	t.Cleanup(func() {
		killSessionProcessGroup = originalKill
		isolatedSessionStopTimeout = originalTimeout
	})

	processWaited := make(chan struct{})
	close(processWaited)
	var killCalls int
	killSessionProcessGroup = func(int) error {
		killCalls++
		return nil
	}
	isolatedSessionStopTimeout = 20 * time.Millisecond
	session := &isolatedSession{
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		processWaited: processWaited,
		// Keep the lifecycle drain pending to reproduce the stale-PID window:
		// cmd.Wait is complete, but the session is not fully reaped yet.
		doneCh: make(chan struct{}),
	}

	err := session.stop()
	if killCalls != 0 {
		t.Fatalf("stop signalled a process group after cmd.Wait: calls=%d", killCalls)
	}
	if !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("stop error = %v, want %v", err, ErrSessionTeardownTimeout)
	}
}

func TestIsolatedSessionExitBarrierSerializesProcessGroupSignal(t *testing.T) {
	originalKill := killSessionProcessGroup
	t.Cleanup(func() {
		killSessionProcessGroup = originalKill
	})

	killEntered := make(chan struct{})
	releaseKill := make(chan struct{})
	killSessionProcessGroup = func(int) error {
		close(killEntered)
		<-releaseKill
		return nil
	}
	session := &isolatedSession{
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		processWaited: make(chan struct{}),
	}

	killDone := make(chan error, 1)
	go func() {
		killDone <- session.killProcessGroupIfRunning()
	}()
	<-killEntered

	barrierStarted := make(chan struct{})
	barrierDone := make(chan struct{})
	go func() {
		close(barrierStarted)
		session.markProcessExitedBeforeReap(nil)
		close(barrierDone)
	}()
	<-barrierStarted
	select {
	case <-barrierDone:
		t.Fatal("exit barrier bypassed an in-flight process-group signal")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseKill)
	if err := <-killDone; err != nil {
		t.Fatal(err)
	}
	<-barrierDone

	// Once the pre-reap barrier publishes exit, no later signal may target
	// the now-reusable numeric group identity. The hook would panic by closing
	// killEntered a second time if this regressed.
	if err := session.killProcessGroupIfRunning(); err != nil {
		t.Fatal(err)
	}
}

func TestIsolatedSessionStopIgnoresKillFailureAfterConfirmedDrain(t *testing.T) {
	originalKill := killSessionProcessGroup
	t.Cleanup(func() {
		killSessionProcessGroup = originalKill
	})
	killCalled := make(chan struct{})
	killSessionProcessGroup = func(int) error {
		close(killCalled)
		return syscall.EPERM
	}

	processWaited := make(chan struct{})
	doneCh := make(chan struct{})
	session := &isolatedSession{
		cmd:           &exec.Cmd{Process: &os.Process{Pid: 424242}},
		processWaited: processWaited,
		doneCh:        doneCh,
	}
	go func() {
		<-killCalled
		close(processWaited)
		close(doneCh)
	}()

	if err := session.stop(); err != nil {
		t.Fatalf("confirmed teardown returned transient kill error: %v", err)
	}
}

func TestIsolatedSessionStopIsIdempotent(t *testing.T) {
	useDirectProcessKill(t)

	lifecycle := newLifecycleHarness()
	isolator := &lifecycleHarnessIsolator{lifecycle: lifecycle}
	session := newLifecycleRuntimeSession(t, isolator, nil)
	if err := session.start(); err != nil {
		t.Fatal(err)
	}

	if err := session.stop(); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := session.stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}
