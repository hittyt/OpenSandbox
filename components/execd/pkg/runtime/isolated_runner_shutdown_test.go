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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

type shutdownTestIsolator struct {
	wrap func(*exec.Cmd, isolation.WrapOptions) error
}

func (*shutdownTestIsolator) Name() string    { return "shutdown-test" }
func (*shutdownTestIsolator) Available() bool { return true }
func (*shutdownTestIsolator) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{
		Available:              true,
		SetprivAvailable:       true,
		SetprivSwitchAvailable: true,
		UsernsAvailable:        true,
	}
}
func (s *shutdownTestIsolator) Wrap(cmd *exec.Cmd, opts isolation.WrapOptions) error {
	if s.wrap != nil {
		return s.wrap(cmd, opts)
	}
	return nil
}
func (s *shutdownTestIsolator) WrapWithLifecycle(
	cmd *exec.Cmd,
	opts isolation.WrapOptions,
) (isolation.WorkloadLifecycle, error) {
	if err := s.Wrap(cmd, opts); err != nil {
		return nil, err
	}
	return newShutdownTestLifecycle(), nil
}

type shutdownTestLifecycle struct {
	done chan struct{}
}

func newShutdownTestLifecycle() *shutdownTestLifecycle {
	done := make(chan struct{})
	close(done)
	return &shutdownTestLifecycle{done: done}
}

func (*shutdownTestLifecycle) WaitForIdentity(context.Context) (isolation.WorkloadIdentity, error) {
	return isolation.WorkloadIdentity{
		PID:                   2,
		SandboxPID:            1,
		NetNamespaceID:        1,
		ProcessStartTimeTicks: 1,
	}, nil
}
func (*shutdownTestLifecycle) MarkReady() error             { return nil }
func (*shutdownTestLifecycle) Abort()                       {}
func (l *shutdownTestLifecycle) DrainDone() <-chan struct{} { return l.done }
func (*shutdownTestLifecycle) DrainError() error            { return nil }
func (*shutdownTestLifecycle) ExitCode() (int, bool)        { return 0, true }
func (*shutdownTestLifecycle) Close() error                 { return nil }

func newShutdownTestRunner(t *testing.T, isolator isolation.Isolator) *IsolatedRunner {
	t.Helper()
	runner, err := NewIsolatedRunner(
		NewController("", ""),
		isolator,
		isolation.Config{
			UpperRoot:     t.TempDir(),
			UpperMaxBytes: 8 << 30,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = runner.Close()
	})
	return runner
}

func shutdownLoopbackOnly() *bool {
	value := false
	return &value
}

func TestIsolatedRunnerCloseStopsAdmissionAndCleansSessions(t *testing.T) {
	runner := newShutdownTestRunner(t, &shutdownTestIsolator{})
	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "workspace"),
		WorkspaceMode: "overlay",
		ShareNet:      shutdownLoopbackOnly(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session := runner.lookup(id)
	if session == nil {
		t.Fatal("created session is missing")
	}
	upperParent := filepath.Dir(session.upperDir)

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- runner.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	select {
	case <-runner.gcDone:
	default:
		t.Fatal("Close returned before the GC goroutine stopped")
	}
	if runner.Available() {
		t.Fatal("closed runner reports available")
	}
	if runner.lookup(id) != nil {
		t.Fatal("session survived Close")
	}
	if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
		t.Fatalf("upper directory survived Close: %v", err)
	}

	rejectedWorkspace := filepath.Join(t.TempDir(), "must-not-exist")
	if _, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: rejectedWorkspace,
		WorkspaceMode: "rw",
		ShareNet:      shutdownLoopbackOnly(),
	}); !errors.Is(err, ErrIsolatedRunnerClosed) {
		t.Fatalf("create after Close error = %v, want ErrIsolatedRunnerClosed", err)
	}
	if _, err := os.Stat(rejectedWorkspace); !os.IsNotExist(err) {
		t.Fatalf("rejected create made workspace: %v", err)
	}

	runner.StopGC()
	runner.StopGC()
}

func TestIsolatedRunnerCloseWaitsForAdmittedCreate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	isolator := &shutdownTestIsolator{
		wrap: func(_ *exec.Cmd, _ isolation.WrapOptions) error {
			startedOnce.Do(func() { close(started) })
			<-release
			return nil
		},
	}
	runner := newShutdownTestRunner(t, isolator)

	type createResult struct {
		id  string
		err error
	}
	createDone := make(chan createResult, 1)
	go func() {
		id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
			WorkspacePath: filepath.Join(t.TempDir(), "workspace"),
			WorkspaceMode: "rw",
			ShareNet:      shutdownLoopbackOnly(),
		})
		createDone <- createResult{id: id, err: err}
	}()
	<-started

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- runner.Close()
	}()

	deadline := time.Now().Add(time.Second)
	for runner.Available() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if runner.Available() {
		t.Fatal("Close did not close admission")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before admitted create completed: %v", err)
	default:
	}

	close(release)
	result := <-createDone
	if result.err != nil {
		t.Fatalf("admitted create failed: %v", result.err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if runner.lookup(result.id) != nil {
		t.Fatal("session created during shutdown survived Close")
	}
}

func TestIsolatedRunnerCloseStartsAllSessionTeardownsConcurrently(t *testing.T) {
	runner := newShutdownTestRunner(t, &shutdownTestIsolator{})

	const sessionCount = 6
	sessions := make([]*isolatedSession, 0, sessionCount)
	releases := make([]func(), 0, sessionCount)
	for i := range sessionCount {
		id := fmt.Sprintf("leased-session-%d", i)
		session := newIsolatedSession(
			id,
			&IsolatedSessionOptions{
				WorkspacePath: t.TempDir(),
				WorkspaceMode: "rw",
			},
			&shutdownTestIsolator{},
		)
		session.operationMu.Lock()
		session.operationCount = 1
		session.operationMu.Unlock()
		runner.ctrl.isolatedSessionMap.Store(id, session)
		sessions = append(sessions, session)

		var releaseOnce sync.Once
		releases = append(releases, func() {
			releaseOnce.Do(func() {
				session.operationMu.Lock()
				session.operationCount--
				if !session.operationOpen && session.operationCount == 0 {
					session.operationDone.Do(func() {
						close(session.operationDrained)
					})
				}
				session.operationMu.Unlock()
			})
		})
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- runner.Close()
	}()

	deadline := time.Now().Add(time.Second)
	for {
		allStarted := true
		for _, session := range sessions {
			if session.deleteMu.TryLock() {
				session.deleteMu.Unlock()
				allStarted = false
				break
			}
		}
		if allStarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close serialized per-session drain waits")
		}
		time.Sleep(time.Millisecond)
	}

	for _, release := range releases {
		release()
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close after concurrent drains: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after every operation released")
	}
}

func TestCreateIsolatedSessionImmediateExitRemovesUpper(t *testing.T) {
	var upperDir string
	isolator := &shutdownTestIsolator{
		wrap: func(cmd *exec.Cmd, opts isolation.WrapOptions) error {
			upperDir = opts.UpperDir
			cmd.Args = []string{cmd.Path, "-c", "exit 17"}
			return nil
		},
	}
	runner := newShutdownTestRunner(t, isolator)

	id, err := runner.CreateIsolatedSession(&IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "workspace"),
		WorkspaceMode: "overlay",
		ShareNet:      shutdownLoopbackOnly(),
	})
	if err == nil || !strings.Contains(err.Error(), "exited immediately") {
		t.Fatalf("create error = %v, want immediate-exit failure", err)
	}
	if id != "" {
		t.Fatalf("failed create returned session ID %q", id)
	}
	if upperDir == "" {
		t.Fatal("overlay upper directory was not allocated")
	}
	if _, err := os.Stat(filepath.Dir(upperDir)); !os.IsNotExist(err) {
		t.Fatalf("failed create leaked upper directory: %v", err)
	}
}

func TestIsolatedRunnerCloseCollectsReleasedUpper(t *testing.T) {
	runner := newShutdownTestRunner(t, &shutdownTestIsolator{})
	id, upper, _, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	runner.upperMgr.Release(id)
	upperParent := filepath.Dir(upper)

	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upperParent); !os.IsNotExist(err) {
		t.Fatalf("Close did not collect released upper directory: %v", err)
	}
}

func TestDeleteTimeoutRetainsResourcesForCloseRetry(t *testing.T) {
	runner := newShutdownTestRunner(t, &shutdownTestIsolator{})
	id := "timeout-session"
	session := newIsolatedSession(
		id,
		&IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "overlay"},
		&shutdownTestIsolator{},
	)
	upperID, upper, work, err := runner.upperMgr.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	session.upperID = upperID
	session.upperDir = upper
	session.workDir = work
	session.cmd = &exec.Cmd{Process: &os.Process{Pid: 12345}}
	runner.ctrl.isolatedSessionMap.Store(id, session)

	originalTimeout := isolatedSessionStopTimeout
	originalKill := killSessionProcessGroup
	t.Cleanup(func() {
		isolatedSessionStopTimeout = originalTimeout
		killSessionProcessGroup = originalKill
	})
	isolatedSessionStopTimeout = 20 * time.Millisecond
	killSessionProcessGroup = func(int) error { return nil }

	startedAt := time.Now()
	err = runner.DeleteIsolatedSession(id)
	if !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("Delete error = %v, want ErrSessionTeardownTimeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Delete exceeded its teardown bound: %v", elapsed)
	}
	if runner.lookup(id) != session {
		t.Fatal("timed-out delete removed the session registration")
	}
	if _, err := os.Stat(filepath.Dir(upper)); err != nil {
		t.Fatalf("timed-out delete removed the upper directory: %v", err)
	}

	close(session.doneCh)
	if err := runner.Close(); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
	if runner.lookup(id) != nil {
		t.Fatal("Close retry left the session registered")
	}
	if _, err := os.Stat(filepath.Dir(upper)); !os.IsNotExist(err) {
		t.Fatalf("Close retry left the upper directory: %v", err)
	}
}
