package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/provision"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
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

// TestEmitProvisionJSON checks the machine-readable contract the init
// script consumes: valid JSON carrying the effective token + per-step
// results, and a non-nil error (non-zero exit) when a step failed.
func TestEmitProvisionJSON(t *testing.T) {
	res := provision.Result{
		Key:          pbserver.VaultKey{Profile: "personal", Vault: "memory"},
		Bucket:       "personal-archives",
		IndexPrefix:  "personal_",
		Token:        "pb_deadbeef",
		TokenCreated: true,
	}
	steps := []provision.StepResult{
		{Name: "binding config", Result: "created"},
		{Name: "config validate", Result: "ok"},
	}
	buf := &bytes.Buffer{}
	if err := emitProvisionJSON(buf, res, steps); err != nil {
		t.Fatalf("clean result should not error: %v", err)
	}
	var got struct {
		Profile string `json:"profile"`
		Token   string `json:"token"`
		OK      bool   `json:"ok"`
		Steps   []struct {
			Name, Result string
		} `json:"steps"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.Profile != "personal" || got.Token != "pb_deadbeef" || !got.OK || len(got.Steps) != 2 {
		t.Fatalf("unexpected JSON view: %+v", got)
	}

	// A failed step flips ok=false and returns a non-nil error.
	buf.Reset()
	fail := append(steps, provision.StepResult{Name: "postgres db", Result: "failed", Err: os.ErrPermission})
	if err := emitProvisionJSON(buf, res, fail); err == nil {
		t.Fatal("a failed step must return a non-nil error")
	}
	if !strings.Contains(buf.String(), `"ok": false`) {
		t.Errorf("failed run should report ok=false:\n%s", buf.String())
	}
}

func errFromString(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
