package siding

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestWithProjectOperationSerializesAndHonorsCancellation(t *testing.T) {
	configDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithProjectOperation(context.Background(), configDir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	err := WithProjectOperation(ctx, configDir, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithProjectOperation() error = %v; want deadline exceeded", err)
	}
	if called {
		t.Fatal("contending operation ran without acquiring the project lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first operation failed: %v", err)
	}
}

func TestDifferentSidingOperationsRunTogether(t *testing.T) {
	configDir := t.TempDir()
	alphaEntered := make(chan struct{})
	releaseAlpha := make(chan struct{})
	alphaDone := make(chan error, 1)
	go func() {
		alphaDone <- WithSidingOperation(context.Background(), configDir, "alpha", func() error {
			close(alphaEntered)
			<-releaseAlpha
			return nil
		})
	}()
	<-alphaEntered

	betaEntered := make(chan struct{})
	betaDone := make(chan error, 1)
	go func() {
		betaDone <- WithSidingOperation(context.Background(), configDir, "beta", func() error {
			close(betaEntered)
			return nil
		})
	}()
	select {
	case <-betaEntered:
	case <-time.After(time.Second):
		t.Fatal("different siding was blocked by long lifecycle work")
	}
	close(releaseAlpha)
	if err := <-alphaDone; err != nil {
		t.Fatal(err)
	}
	if err := <-betaDone; err != nil {
		t.Fatal(err)
	}
}

func TestSameSidingOperationSerializes(t *testing.T) {
	configDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithSidingOperation(context.Background(), configDir, "alpha", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	err := WithSidingOperation(ctx, configDir, "alpha", func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("contending same-siding operation = called %v, error %v", called, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProjectExclusiveOperationsBlockLifecycleAndEachOther(t *testing.T) {
	configDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithProjectOperation(context.Background(), configDir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	for _, test := range []struct {
		name string
		run  func(context.Context, func() error) error
	}{
		{name: "start", run: func(ctx context.Context, fn func() error) error {
			return WithSidingOperation(ctx, configDir, "alpha", fn)
		}},
		{name: "stop", run: func(ctx context.Context, fn func() error) error {
			return WithSidingOperation(ctx, configDir, "alpha", fn)
		}},
		{name: "switch", run: func(ctx context.Context, fn func() error) error {
			return WithProjectOperation(ctx, configDir, fn)
		}},
		{name: "promotion", run: func(ctx context.Context, fn func() error) error {
			return WithProjectSidingOperation(ctx, configDir, "alpha", fn)
		}},
		{name: "app registration", run: func(ctx context.Context, fn func() error) error {
			return WithProjectOperation(ctx, configDir, fn)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			called := false
			err := test.run(ctx, func() error { called = true; return nil })
			if !errors.Is(err, context.DeadlineExceeded) || called {
				t.Fatalf("contender = called %v, error %v", called, err)
			}
		})
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSidingStateMergesPreserveEveryWriter(t *testing.T) {
	configDir := t.TempDir()
	app := state.App{ConfigDir: configDir, Sidings: map[string]state.Siding{
		"alpha": {Name: "alpha"},
		"beta":  {Name: "beta"},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sd := range []state.Siding{{Name: "alpha", LastIP: "10.0.0.1"}, {Name: "beta", LastIP: "10.0.0.2"}} {
		sd := sd
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := MergeSidingState(context.Background(), configDir, sd, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sidings["alpha"].LastIP != "10.0.0.1" || got.Sidings["beta"].LastIP != "10.0.0.2" {
		t.Fatalf("concurrent state merge lost a writer: %#v", got.Sidings)
	}
}

func TestMergeSidingStatePreservesFreshFields(t *testing.T) {
	configDir := t.TempDir()
	app := state.App{ConfigDir: configDir, Memory: "4g", Sidings: map[string]state.Siding{
		"one": {Name: "one", LastIP: "old"},
		"two": {Name: "two", LastIP: "keep"},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	app.Memory = "8g"
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}

	merged, err := MergeSidingState(context.Background(), configDir, state.Siding{Name: "one", LastIP: "new"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Memory != "8g" || merged.Sidings["two"].LastIP != "keep" || merged.Sidings["one"].LastIP != "new" {
		t.Fatalf("merged state lost a concurrent field: %#v", merged)
	}
}
