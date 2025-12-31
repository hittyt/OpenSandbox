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

package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGetCommandStatus_NotFound(t *testing.T) {
	c := NewController("", "")

	if _, err := c.GetCommandStatus("missing"); err == nil {
		t.Fatalf("expected error for missing session")
	}
}

func TestGetCommandStatus_Running(t *testing.T) {
	c := NewController("", "")

	session := "sess-running"
	started := time.Now().Add(-time.Second)
	kernel := &commandKernel{
		pid:        123,
		stdoutPath: filepath.Join(os.TempDir(), session+".stdout"),
		stderrPath: filepath.Join(os.TempDir(), session+".stderr"),
		startedAt:  started,
		running:    true,
	}
	c.storeCommandKernel(session, kernel)

	status, err := c.GetCommandStatus(session)
	if err != nil {
		t.Fatalf("GetCommandStatus error: %v", err)
	}
	if !status.Running {
		t.Fatalf("expected running=true")
	}
	if status.ExitCode != nil {
		t.Fatalf("expected exitCode to be nil for running command")
	}
	if status.FinishedAt != nil {
		t.Fatalf("expected finishedAt to be nil for running command")
	}
	if !status.StartedAt.Equal(started) {
		t.Fatalf("startedAt mismatch")
	}
}

func TestGetCommandOutput_Completed(t *testing.T) {
	c := NewController("", "")

	tmpDir := t.TempDir()
	session := "sess-done"
	stdoutPath := filepath.Join(tmpDir, session+".stdout")

	stdoutContent := "hello stdout"
	if err := os.WriteFile(stdoutPath, []byte(stdoutContent), 0o644); err != nil {
		t.Fatalf("write stdout: %v", err)
	}

	started := time.Now().Add(-2 * time.Second)
	finished := time.Now()
	exitCode := 0
	kernel := &commandKernel{
		pid:          456,
		stdoutPath:   stdoutPath,
		isBackground: true,
		startedAt:    started,
		finishedAt:   &finished,
		exitCode:     &exitCode,
		errMsg:       "",
		running:      false,
	}
	c.storeCommandKernel(session, kernel)

	output, cursor, err := c.SeekBackgroundCommandOutput(session, 0)
	if err != nil {
		t.Fatalf("GetCommandOutput error: %v", err)
	}

	if cursor <= 0 {
		t.Fatalf("expected cursor>=0")
	}
	if string(output) != stdoutContent {
		t.Fatalf("expected output=%s, got %s", stdoutContent, string(output))
	}
}
