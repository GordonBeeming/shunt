package cmd

import (
	"context"
	"reflect"
	"testing"

	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestSyncTargets(t *testing.T) {
	app := state.App{
		Name:       "myapp",
		LiveSiding: "beta",
		Sidings: map[string]state.Siding{
			"alpha": {}, "beta": {},
		},
	}
	ctx := context.Background()

	cases := []struct {
		name    string
		loc     resolve.Location
		args    []string
		all     bool
		want    []string
		wantErr bool
	}{
		{"--all lists every siding, sorted", resolve.Location{}, nil, true, []string{"alpha", "beta"}, false},
		{"name arg", resolve.Location{}, []string{"alpha"}, false, []string{"alpha"}, false},
		{"unknown name errors", resolve.Location{}, []string{"nope"}, false, nil, true},
		{"cwd siding", resolve.Location{Siding: "alpha"}, nil, false, []string{"alpha"}, false},
		{"live siding fallback", resolve.Location{}, nil, false, []string{"beta"}, false},
		{"arg beats cwd + live", resolve.Location{Siding: "beta"}, []string{"alpha"}, false, []string{"alpha"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := syncTargets(ctx, app, c.loc, c.args, c.all)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && !reflect.DeepEqual(got, c.want) {
				t.Errorf("targets = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSyncTargetsAllEmpty(t *testing.T) {
	app := state.App{Name: "myapp", Sidings: map[string]state.Siding{}}
	if _, err := syncTargets(context.Background(), app, resolve.Location{}, nil, true); err == nil {
		t.Error("expected an error for --all with no sidings")
	}
}
