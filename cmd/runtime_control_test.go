package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRestartRuntimeWaitsBeyondFormerStartupTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"instance_id":"new-instance"}`)
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(parsed.Host)
	dir := t.TempDir()
	previous := launchRuntimeProcess
	t.Cleanup(func() { launchRuntimeProcess = previous })
	launchRuntimeProcess = func(runtimeRecord) (<-chan error, error) {
		done := make(chan error, 1)
		go func() {
			time.Sleep(31 * time.Second)
			if err := registerRuntimeRecord(dir, runtimeRecord{InstanceID: "new-instance", Port: port}); err != nil {
				done <- err
			}
		}()
		return done, nil
	}
	if err := restartRuntime(dir, runtimeRecord{InstanceID: "old-instance", Port: port}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRuntimeReportsProcessFailureWithoutWaitingForever(t *testing.T) {
	previous := launchRuntimeProcess
	t.Cleanup(func() { launchRuntimeProcess = previous })
	for _, failure := range []error{nil, errors.New("install failed")} {
		launchRuntimeProcess = func(runtimeRecord) (<-chan error, error) {
			done := make(chan error, 1)
			done <- failure
			return done, nil
		}
		err := restartRuntime(t.TempDir(), runtimeRecord{Port: "8080"})
		if err == nil || (failure != nil && !strings.Contains(err.Error(), failure.Error())) {
			t.Fatalf("process failure %v not reported: %v", failure, err)
		}
	}
}
