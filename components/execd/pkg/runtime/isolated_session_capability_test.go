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
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func capabilityTestOptions(t *testing.T, name string) *IsolatedSessionOptions {
	t.Helper()
	shareNet := false
	return &IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), name),
		WorkspaceMode: "rw",
		ShareNet:      &shareNet,
	}
}

func TestCreateIsolatedSessionWithCapabilityStoresOnlyDigest(t *testing.T) {
	runner := newTestRunner(t)
	id, capability, err := runner.CreateIsolatedSessionWithCapability(
		capabilityTestOptions(t, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(id)

	decoded, err := base64.RawURLEncoding.DecodeString(capability)
	if err != nil {
		t.Fatalf("capability is not raw base64url: %v", err)
	}
	if len(decoded) != sessionCapabilityBytes {
		t.Fatalf(
			"decoded capability length = %d, want %d",
			len(decoded),
			sessionCapabilityBytes,
		)
	}
	session := runner.lookup(id)
	if session == nil {
		t.Fatal("session not found after create")
	}
	wantDigest := sha256.Sum256([]byte(capability))
	if session.capabilityDigest != wantDigest {
		t.Fatal("session did not retain the capability digest")
	}

	for name, candidate := range map[string]string{
		"empty":      "",
		"wrong":      strings.Repeat("A", 43),
		"malformed":  strings.Repeat("!", 43),
		"padded":     capability + "=",
		"unknown-id": capability,
	} {
		sessionID := id
		if name == "unknown-id" {
			sessionID = "unknown"
		}
		if err := runner.ValidateSessionCapability(
			sessionID,
			candidate,
		); !errors.Is(err, ErrSessionCapabilityInvalid) {
			t.Fatalf(
				"%s capability error = %v, want ErrSessionCapabilityInvalid",
				name,
				err,
			)
		}
	}
	if err := runner.ValidateSessionCapability(id, capability); err != nil {
		t.Fatalf("valid capability rejected: %v", err)
	}
}

func TestCreateIsolatedSessionCapabilityRandomFailureHasNoSideEffects(
	t *testing.T,
) {
	previousRandom := readSessionCapabilityRandom
	readSessionCapabilityRandom = func([]byte) (int, error) {
		return 0, errors.New("random source unavailable")
	}
	t.Cleanup(func() {
		readSessionCapabilityRandom = previousRandom
	})

	runner := newTestRunner(t)
	opts := capabilityTestOptions(t, "must-not-exist")
	workspace := opts.WorkspacePath
	id, capability, err := runner.CreateIsolatedSessionWithCapability(opts)
	if err == nil || !strings.Contains(err.Error(), "random source unavailable") {
		t.Fatalf("error = %v, want random source failure", err)
	}
	if id != "" || capability != "" {
		t.Fatalf("failed create returned credentials: %q/%q", id, capability)
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Fatalf("random failure created workspace: %v", statErr)
	}
	if runner.lookup(id) != nil {
		t.Fatal("random failure stored a session")
	}
}

func TestGenerateSessionCapabilityRejectsShortRandomRead(t *testing.T) {
	previousRandom := readSessionCapabilityRandom
	readSessionCapabilityRandom = func(buffer []byte) (int, error) {
		return len(buffer) - 1, nil
	}
	t.Cleanup(func() {
		readSessionCapabilityRandom = previousRandom
	})

	capability, digest, err := generateSessionCapability()
	if err == nil || !strings.Contains(err.Error(), "short random read") {
		t.Fatalf("error = %v, want short random read", err)
	}
	if capability != "" || digest != ([sha256.Size]byte{}) {
		t.Fatal("short random read returned capability material")
	}
}

func TestCreateIsolatedSessionCapabilitiesAreSessionBound(t *testing.T) {
	runner := newTestRunner(t)
	idA, capabilityA, err := runner.CreateIsolatedSessionWithCapability(
		capabilityTestOptions(t, "workspace-a"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(idA)

	idB, capabilityB, err := runner.CreateIsolatedSessionWithCapability(
		capabilityTestOptions(t, "workspace-b"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.DeleteIsolatedSession(idB)

	if capabilityA == capabilityB {
		t.Fatal("two sessions received the same capability")
	}
	if err := runner.ValidateSessionCapability(
		idA,
		capabilityB,
	); !errors.Is(err, ErrSessionCapabilityInvalid) {
		t.Fatalf(
			"cross-session capability error = %v, want ErrSessionCapabilityInvalid",
			err,
		)
	}
}

func TestBeginDeleteRevokesAdmissionAndWaitsForOperations(t *testing.T) {
	runner := newTestRunner(t)
	id, capability, err := runner.CreateIsolatedSessionWithCapability(
		capabilityTestOptions(t, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}

	release, err := runner.AcquireSessionOperation(id, capability)
	if err != nil {
		t.Fatalf("acquire operation: %v", err)
	}
	deleteSession, err := runner.BeginDeleteIsolatedSession(id, capability)
	if err != nil {
		t.Fatalf("begin delete: %v", err)
	}
	if _, err := runner.AcquireSessionOperation(
		id,
		capability,
	); !errors.Is(err, ErrSessionCapabilityInvalid) {
		t.Fatalf("revoked admission error = %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- deleteSession()
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("delete completed before operation release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	release()
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("delete after operation release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not complete after operation release")
	}
	if err := deleteSession(); err != nil {
		t.Fatalf("authorized delete closure is not idempotent: %v", err)
	}
}

func TestDeleteOperationDrainTimeoutRetainsSessionForCleanupRetry(t *testing.T) {
	previousTimeout := isolatedSessionOperationDrainTimeout
	isolatedSessionOperationDrainTimeout = 25 * time.Millisecond
	t.Cleanup(func() {
		isolatedSessionOperationDrainTimeout = previousTimeout
	})

	runner := newTestRunner(t)
	id, capability, err := runner.CreateIsolatedSessionWithCapability(
		capabilityTestOptions(t, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	release, err := runner.AcquireSessionOperation(id, capability)
	if err != nil {
		t.Fatalf("acquire operation: %v", err)
	}
	deleteSession, err := runner.BeginDeleteIsolatedSession(id, capability)
	if err != nil {
		t.Fatalf("begin delete: %v", err)
	}

	if err := deleteSession(); !errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf(
			"delete error = %v, want ErrSessionTeardownTimeout",
			err,
		)
	}
	if runner.lookup(id) == nil {
		t.Fatal("timed-out delete removed the retryable session")
	}

	release()
	if err := deleteSession(); errors.Is(err, ErrSessionTeardownTimeout) {
		t.Fatalf("retry still timed out after operation drain: %v", err)
	}
	if runner.lookup(id) != nil {
		t.Fatal("successful retry kept the session registered")
	}
}
