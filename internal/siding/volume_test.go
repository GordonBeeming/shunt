package siding

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestCreateBindVolumesPassesMountPathAsArgument(t *testing.T) {
	originalExec := execGuest
	defer func() { execGuest = originalExec }()
	app := state.App{Volumes: []string{"pg-data"}}
	sd := state.Siding{Container: "guest"}
	var calls [][]string
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch len(calls) {
		case 1:
			if strings.Contains(args[2], "/mnt/dvol/pg-data") {
				t.Fatal("mount path was concatenated into the probe script")
			}
			if args[len(args)-1] != "/mnt/dvol/pg-data" {
				t.Fatalf("probe args = %#v", args)
			}
			return "directory\n", nil
		case 2:
			return "", nil
		case 3:
			return "pg-data\n", nil
		default:
			t.Fatalf("unexpected call %#v", args)
			return "", nil
		}
	}
	if err := CreateBindVolumes(context.Background(), app, sd); err != nil {
		t.Fatal(err)
	}
	wantCreate := []string{"docker", "volume", "create", "--driver", "local", "--opt", "type=none", "--opt", "o=bind", "--opt", "device=/mnt/dvol/pg-data", "pg-data"}
	if !reflect.DeepEqual(calls[2], wantCreate) {
		t.Fatalf("create args = %#v, want %#v", calls[2], wantCreate)
	}
}

func TestCreateBindVolumesDistinguishesAbsentMount(t *testing.T) {
	originalExec := execGuest
	defer func() { execGuest = originalExec }()
	calls := 0
	execGuest = func(context.Context, string, ...string) (string, error) {
		calls++
		return "absent\n", nil
	}
	if err := CreateBindVolumes(context.Background(), state.App{Volumes: []string{"pg-data"}}, state.Siding{Container: "guest"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("absent mount made %d guest calls, want 1", calls)
	}
}

func TestCreateBindVolumesSurfacesGuestProbeFailures(t *testing.T) {
	tests := []struct {
		name      string
		responses []struct {
			out string
			err error
		}
		want string
	}{
		{
			name: "mount probe",
			responses: []struct {
				out string
				err error
			}{{err: errors.New("guest unavailable")}},
			want: "probe host-backed mount",
		},
		{
			name: "Docker volume probe",
			responses: []struct {
				out string
				err error
			}{{out: "directory"}, {err: errors.New("daemon unavailable")}},
			want: "probe Docker volume",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalExec := execGuest
			defer func() { execGuest = originalExec }()
			call := 0
			execGuest = func(context.Context, string, ...string) (string, error) {
				response := test.responses[call]
				call++
				return response.out, response.err
			}
			err := CreateBindVolumes(context.Background(), state.App{Volumes: []string{"pg-data"}}, state.Siding{Container: "guest"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CreateBindVolumes() error = %v", err)
			}
		})
	}
}

func TestCreateBindVolumesRejectsInvalidEngineNameBeforeGuestExec(t *testing.T) {
	originalExec := execGuest
	defer func() { execGuest = originalExec }()
	called := false
	execGuest = func(context.Context, string, ...string) (string, error) {
		called = true
		return "", nil
	}
	err := CreateBindVolumes(context.Background(), state.App{Volumes: []string{"pg:data"}}, state.Siding{Container: "guest"})
	if err == nil {
		t.Fatal("CreateBindVolumes accepted an invalid Docker volume name")
	}
	if called {
		t.Fatal("guest exec ran before volume validation")
	}
}
