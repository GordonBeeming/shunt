package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardResponding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := dashboardResponding(context.Background(), server.URL); err != nil {
		t.Fatalf("dashboardResponding returned %v, want nil", err)
	}
}

func TestDashboardRespondingRejectsUnhealthyServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := dashboardResponding(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("dashboardResponding error = %v, want 503 status", err)
	}
}

func TestWaitForDashboardHonorsTimeoutDuringRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	err := waitForDashboard(context.Background(), server.URL, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForDashboard error = %v, want context deadline exceeded", err)
	}
}

func TestWaitForDashboardReportsTimeoutAndLastHealthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := waitForDashboard(context.Background(), server.URL, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("waitForDashboard error = %v, want timeout with last 503 status", err)
	}
}

func TestWaitForDashboardHonorsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForDashboard(ctx, "http://localhost:1", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForDashboard error = %v, want context canceled", err)
	}
}
