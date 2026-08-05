package siding

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/state"
)

var (
	assureImageSources  = imagecache.AssureSources
	refreshImageSources = imagecache.RefreshSources
)

func localBuildSources(app state.App) []imagecache.LocalBuildSource {
	result := make([]imagecache.LocalBuildSource, 0, len(app.PrebakeBuilds))
	for _, build := range app.PrebakeBuilds {
		args := make(map[string]string, len(build.BuildArgs))
		for key, value := range build.BuildArgs {
			args[key] = value
		}
		result = append(result, imagecache.LocalBuildSource{
			Ref:        build.Image,
			ContextDir: build.Context,
			Dockerfile: build.Dockerfile,
			Platform:   build.Platform,
			BuildArgs:  args,
		})
	}
	return result
}

func configuredImageRefs(app state.App) ([]string, error) {
	refs := make([]string, 0, len(app.PrebakeImages)+len(app.PrebakeBuilds))
	refs = append(refs, app.PrebakeImages...)
	for _, build := range app.PrebakeBuilds {
		refs = append(refs, build.Image)
	}
	canonical := make([]string, 0, len(refs))
	for _, ref := range refs {
		parsed, err := name.ParseReference(ref)
		if err != nil {
			return nil, fmt.Errorf("parse configured image %q: %w", ref, err)
		}
		canonical = append(canonical, parsed.Name())
	}
	sort.Strings(canonical)
	return canonical, nil
}

// RefreshImageCache refreshes registry tags and rebuilds local declarations.
func RefreshImageCache(ctx context.Context, app state.App) ([]imagecache.Change, error) {
	return refreshImageSources(ctx, WarmTarPath(app), app.PrebakeImages, localBuildSources(app))
}

// RefreshImageCacheProgress refreshes cache sources and reports safe per-image
// progress for the CLI. Registry credentials and bearer realms never cross this
// package boundary.
func RefreshImageCacheProgress(ctx context.Context, app state.App, progress func(imagecache.ProgressEvent)) ([]imagecache.Change, error) {
	return imagecache.RefreshSourcesProgress(ctx, WarmTarPath(app), app.PrebakeImages, localBuildSources(app), progress)
}
