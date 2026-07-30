package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/provision"
)

// TestReportSteps_ExitsNonZeroOnFailure locks the aggregate-and-report
// contract: any failed step yields a non-nil error (non-zero exit),
// while an all-clean run returns nil. (The provisioning steps themselves
// are tested in internal/provision.)
func TestReportSteps_ExitsNonZeroOnFailure(t *testing.T) {
	buf := &bytes.Buffer{}
	err := reportSteps(buf, []provision.StepResult{
		{Name: "a", Result: "ok"},
		{Name: "b", Result: "failed", Err: os.ErrPermission},
	})
	if err == nil {
		t.Fatal("expected non-nil error when a step failed")
	}

	buf.Reset()
	if err := reportSteps(buf, []provision.StepResult{{Name: "a", Result: "ok"}, {Name: "b", Result: "exists"}}); err != nil {
		t.Fatalf("all-clean run should return nil, got %v", err)
	}
	if !strings.Contains(buf.String(), "binding live.") {
		t.Errorf("clean run should print success banner, got:\n%s", buf.String())
	}
}

// TestAnnotatePGConnectErr adds the host/container hint only to
// connection-shaped failures, and leaves other errors untouched.
func TestAnnotatePGConnectErr(t *testing.T) {
	connErr := annotatePGConnectErr(errFromString("failed to connect: dial tcp 127.0.0.1:1: connection refused"))
	if !strings.Contains(connErr.Error(), "localhost:5433") {
		t.Errorf("connect error missing host hint: %v", connErr)
	}
	plain := annotatePGConnectErr(errFromString("migration checksum mismatch"))
	if strings.Contains(plain.Error(), "localhost:5433") {
		t.Errorf("non-connect error should not get the hint: %v", plain)
	}
}

func errFromString(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
