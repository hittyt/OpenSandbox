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
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/log"
)

// IsolatedSessionOptions bundles the parameters for creating an isolated session.
type IsolatedSessionOptions struct {
	Profile            string
	WorkspacePath      string
	WorkspaceMode      string
	ExtraWritable      []string
	Binds              []isolation.BindMount
	ShareNet           *bool
	EnvPassthroughMode string
	EnvPassthroughKeys []string
	Uid                *uint32
	Gid                *uint32
	UidMode            string // "setpriv" (default) or "userns"
	IdleTimeoutSeconds int
}

// isolatedSession holds a long-running shell process inside a bwrap namespace.
type isolatedSession struct {
	id                   string
	capabilityDigest     [sha256.Size]byte
	mu                   sync.RWMutex
	runMu                sync.Mutex // serializes concurrent Run calls
	operationMu          sync.Mutex
	operationCount       int
	operationOpen        bool
	operationDrained     chan struct{}
	operationDone        sync.Once
	deleteMu             sync.Mutex
	deleteComplete       bool
	deleteErr            error
	opts                 *IsolatedSessionOptions
	cmd                  *exec.Cmd
	stdin                io.WriteCloser
	stdout               io.ReadCloser
	processWaited        chan struct{} // closed immediately after cmd.Wait returns
	processSignalMu      sync.Mutex    // serializes process-group signals with the pre-reap barrier
	processExited        bool          // set before Linux can reap and reuse the numeric PID/PGID
	doneCh               chan struct{} // closed after process wait and lifecycle drain
	lifecycleMonitorDone chan struct{} // closed after drain-failure monitor exits
	upperID              string        // key in UpperManager, used for Release/Remove
	upperDir             string
	workDir              string
	createdAt            time.Time
	lastRunAt            time.Time
	isolator             isolation.Isolator
	lifecycle            isolation.WorkloadLifecycle
	identity             isolation.WorkloadIdentity
}

const isolatedSessionStartupTimeout = 10 * time.Second

var (
	isolatedSessionStopTimeout = 5 * time.Second
	killSessionProcessGroup    = func(pid int) error {
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
)

func newIsolatedSession(
	id string,
	opts *IsolatedSessionOptions,
	iso isolation.Isolator,
) *isolatedSession {
	return newIsolatedSessionWithCapabilityDigest(
		id,
		[sha256.Size]byte{},
		opts,
		iso,
	)
}

func newIsolatedSessionWithCapabilityDigest(
	id string,
	capabilityDigest [sha256.Size]byte,
	opts *IsolatedSessionOptions,
	iso isolation.Isolator,
) *isolatedSession {
	return &isolatedSession{
		id:               id,
		capabilityDigest: capabilityDigest,
		opts:             opts,
		isolator:         iso,
		processWaited:    make(chan struct{}),
		doneCh:           make(chan struct{}),
		operationOpen:    true,
		operationDrained: make(chan struct{}),
		createdAt:        time.Now(),
		lastRunAt:        time.Now(),
	}
}

// start launches bwrap and the preferred shell inside a namespace.
func (s *isolatedSession) start() error {
	shell := getShell()
	var args []string
	if shell == bashShell {
		args = append(args, "--noprofile", "--norc")
	}
	cmd := exec.Command(shell, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	wrapOpts := isolation.WrapOptions{
		ExtraWritable: s.opts.ExtraWritable,
		Binds:         s.opts.Binds,
		ShareNet:      true,
	}

	switch s.opts.Profile {
	case string(isolation.ProfileBalanced):
		wrapOpts.Profile = isolation.ProfileBalanced
	case string(isolation.ProfileStrict), "":
		wrapOpts.Profile = isolation.ProfileStrict
	default:
		return fmt.Errorf("unknown isolation profile %q", s.opts.Profile)
	}

	wrapOpts.Workspace.Path = s.opts.WorkspacePath
	switch isolation.WorkspaceMode(s.opts.WorkspaceMode) {
	case isolation.WorkspaceRW:
		wrapOpts.Workspace.Mode = isolation.WorkspaceRW
	case isolation.WorkspaceRO:
		wrapOpts.Workspace.Mode = isolation.WorkspaceRO
	default:
		wrapOpts.Workspace.Mode = isolation.WorkspaceOverlay
	}

	if s.opts.ShareNet != nil {
		wrapOpts.ShareNet = *s.opts.ShareNet
	}
	if s.opts.EnvPassthroughMode != "" {
		wrapOpts.EnvPassthrough.Mode = isolation.EnvMode(s.opts.EnvPassthroughMode)
		wrapOpts.EnvPassthrough.Keys = s.opts.EnvPassthroughKeys
	} else {
		wrapOpts.EnvPassthrough.Mode = isolation.EnvModeDeny
	}
	wrapOpts.Uid = s.opts.Uid
	wrapOpts.Gid = s.opts.Gid
	if s.opts.UidMode != "" {
		wrapOpts.UidMode = isolation.UidMode(s.opts.UidMode)
	}
	wrapOpts.UpperDir = s.upperDir
	wrapOpts.WorkDir = s.workDir

	lifecycleIsolator, ok := s.isolator.(isolation.LifecycleIsolator)
	if !ok {
		return ErrSessionLifecycleUnavailable
	}
	lifecycle, err := lifecycleIsolator.WrapWithLifecycle(cmd, wrapOpts)
	if err != nil {
		closeCommandExtraFiles(cmd)
		if lifecycle != nil {
			lifecycle.Abort()
			return errors.Join(err, lifecycle.Close())
		}
		return err
	}
	if lifecycle == nil {
		closeCommandExtraFiles(cmd)
		return ErrSessionLifecycleUnavailable
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return errors.Join(
			err,
			cleanupUnstartedSession(cmd, lifecycle, nil, nil),
		)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Join(
			err,
			cleanupUnstartedSession(cmd, lifecycle, stdin, nil),
		)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return errors.Join(
			fmt.Errorf("start %s: %w", shell, err),
			cleanupUnstartedSession(cmd, lifecycle, stdin, stdout),
		)
	}

	closeCommandExtraFiles(cmd)

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.lifecycle = lifecycle

	go func() {
		_ = waitCommandWithExitBarrier(cmd, s.markProcessExitedBeforeReap)
		// Publish process reaping before waiting for lifecycle accounting.
		// Once Wait returns, the numeric PID/PGID may be reused and must never
		// be signalled again.
		close(s.processWaited)
		// A session is not fully reaped until bubblewrap's status stream has
		// also reached a validated terminal state.
		<-lifecycle.DrainDone()
		close(s.doneCh)
	}()
	s.lifecycleMonitorDone = make(chan struct{})
	go func() {
		defer close(s.lifecycleMonitorDone)
		<-lifecycle.DrainDone()
		if lifecycle.DrainError() != nil && cmd.Process != nil {
			// Losing trusted lifecycle accounting invalidates the workload
			// identity. Never leave that workload running.
			_ = s.killProcessGroupIfRunning()
		}
	}()

	startupCtx, cancelStartup := context.WithTimeout(
		context.Background(),
		isolatedSessionStartupTimeout,
	)
	identity, err := lifecycle.WaitForIdentity(startupCtx)
	cancelStartup()
	if err != nil {
		return s.failStartup(fmt.Errorf("wait for isolated workload identity: %w", err))
	}
	s.identity = identity

	if err := lifecycle.MarkReady(); err != nil {
		return s.failStartup(fmt.Errorf("release isolated workload gate: %w", err))
	}

	// Brief startup check — if bwrap fails immediately (bad capabilities,
	// missing binary inside namespace, etc.) we detect it here instead of
	// waiting until the first Run call.
	select {
	case <-s.processWaited:
		return s.failStartup(fmt.Errorf("bwrap process exited immediately after start"))
	case <-time.After(100 * time.Millisecond):
	}

	return nil
}

// stop kills the bwrap process group and waits for process reaping.
func (s *isolatedSession) stop() error {
	var cleanupErr error
	var processGroupKillErr error
	processDone := false
	hasProcess := s.cmd != nil && s.cmd.Process != nil
	if hasProcess {
		select {
		case <-s.doneCh:
			processDone = true
		default:
		}
		if !processDone {
			// Signal while the session pipes and lifecycle gate are still
			// open. Closing stdin first lets an interactive shell exit and be
			// reaped just before a group signal, creating a stale-PID window.
			if err := s.killProcessGroupIfRunning(); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				processGroupKillErr = fmt.Errorf(
					"kill isolated session process group: %w",
					err,
				)
			}
		}
	}

	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.stdout != nil {
		_ = s.stdout.Close()
		s.stdout = nil
	}
	if s.lifecycle != nil {
		s.lifecycle.Abort()
	}
	if hasProcess {
		if !processDone {
			timer := time.NewTimer(isolatedSessionStopTimeout)
			select {
			case <-s.doneCh:
				processDone = true
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				cleanupErr = errors.Join(
					cleanupErr,
					processGroupKillErr,
					ErrSessionTeardownTimeout,
				)
			}
		}
		if !processDone {
			// lifecycle.Close may wait for its status-drain goroutine. Do not
			// turn an unreapable workload into an unbounded session teardown.
			return cleanupErr
		}
	}
	if processDone && processGroupKillErr != nil {
		// A group signal can race a naturally exiting or already-zombie
		// leader and return EPERM on some kernels. Once both cmd.Wait and the
		// trusted lifecycle drain are complete, teardown is confirmed and the
		// transient signal error must not turn a successful delete into 500.
		log.Warn("%v; session process and lifecycle are fully reaped", processGroupKillErr)
	}
	if s.lifecycleMonitorDone != nil {
		// DrainDone is closed before doneCh, so the monitor is guaranteed to
		// make progress here. Waiting gives teardown ownership of the monitor
		// and prevents it from outliving the session or test hooks it uses.
		<-s.lifecycleMonitorDone
	}
	if s.lifecycle != nil {
		if err := s.lifecycle.Close(); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("close isolated session lifecycle: %w", err),
			)
		} else {
			s.lifecycle = nil
		}
	}
	return cleanupErr
}

// markProcessExitedBeforeReap publishes the Linux WNOWAIT exit barrier before
// cmd.Wait can reap and release the numeric PID/PGID. If the kernel barrier
// itself fails, disable numeric group signals rather than risk targeting a
// reused identity; the bounded teardown path will force sandbox-level cleanup.
func (s *isolatedSession) markProcessExitedBeforeReap(barrierErr error) {
	s.processSignalMu.Lock()
	defer s.processSignalMu.Unlock()

	if barrierErr != nil {
		log.Error("isolated session exit barrier failed: %v", barrierErr)
	}
	s.processExited = true
}

// killProcessGroupIfRunning serializes the signal syscall with the Linux
// pre-reap exit barrier. The group leader remains an unreaped child while this
// mutex is held, so its numeric PID/PGID cannot be reused for another session.
func (s *isolatedSession) killProcessGroupIfRunning() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	s.processSignalMu.Lock()
	defer s.processSignalMu.Unlock()
	if s.processExited {
		return nil
	}
	select {
	case <-s.processWaited:
		s.processExited = true
		return nil
	default:
	}
	return killSessionProcessGroup(s.cmd.Process.Pid)
}

// dead returns true if the bwrap process has exited.
func (s *isolatedSession) dead() bool {
	select {
	case <-s.doneCh:
		return true
	default:
		return false
	}
}

func (s *isolatedSession) failStartup(startErr error) error {
	if cleanupErr := s.stop(); cleanupErr != nil {
		return errors.Join(startErr, fmt.Errorf("clean up failed session startup: %w", cleanupErr))
	}
	return startErr
}

func cleanupUnstartedSession(
	cmd *exec.Cmd,
	lifecycle isolation.WorkloadLifecycle,
	stdin io.WriteCloser,
	stdout io.ReadCloser,
) error {
	var cleanupErr error
	if stdin != nil {
		if err := stdin.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close isolated session stdin: %w", err))
		}
	}
	if stdout != nil {
		if err := stdout.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close isolated session stdout: %w", err))
		}
	}
	closeCommandExtraFiles(cmd)
	lifecycle.Abort()
	if err := lifecycle.Close(); err != nil {
		cleanupErr = errors.Join(
			cleanupErr,
			fmt.Errorf("close isolated session lifecycle: %w", err),
		)
	}
	return cleanupErr
}

func closeCommandExtraFiles(cmd *exec.Cmd) {
	for _, file := range cmd.ExtraFiles {
		if file != nil {
			_ = file.Close()
		}
	}
	cmd.ExtraFiles = nil
}
