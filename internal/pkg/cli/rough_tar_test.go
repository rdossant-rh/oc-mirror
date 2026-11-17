package cli

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
	"go.podman.io/image/v5/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/oc-mirror/v2/internal/pkg/api/v2alpha1"
	"github.com/openshift/oc-mirror/v2/internal/pkg/archive"
	"github.com/openshift/oc-mirror/v2/internal/pkg/consts"
	"github.com/openshift/oc-mirror/v2/internal/pkg/history"
	"github.com/openshift/oc-mirror/v2/internal/pkg/mirror"
)

func TestRoughTarballSize(t *testing.T) {
	shared := types.BlobInfo{Digest: digest.FromString("shared"), Size: 100}
	unique := types.BlobInfo{Digest: digest.FromString("unique"), Size: 200}
	sig := types.BlobInfo{Digest: digest.FromString("signature"), Size: 300}
	a := v2alpha1.CopyImageSchema{Source: "a", Destination: "same"}
	b := v2alpha1.CopyImageSchema{Source: "b", Destination: "same"}
	for _, tc := range []struct {
		name        string
		images      []v2alpha1.CopyImageSchema
		results     map[string]imageSizeResult
		archived    sets.Set[string]
		metadata    sizeInventory
		canceled    bool
		wantBytes   int64
		wantErrors  int
		unavailable bool
	}{
		{name: "empty selection", wantBytes: 1024},
		{name: "empty file", images: []v2alpha1.CopyImageSchema{a},
			results: map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{{Digest: shared.Digest}}}}, wantBytes: 4096},
		{name: "global shared and signature dedup", images: []v2alpha1.CopyImageSchema{a, a, b},
			results:  map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{shared, unique, shared}}, "b": {Blobs: []types.BlobInfo{shared, sig}}},
			metadata: sizeInventory{Bytes: 142, Files: 2}, wantBytes: 600 + 142 + 5*3072 + 1024},
		{name: "history filters blobs not links", images: []v2alpha1.CopyImageSchema{a, b},
			results:  map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{shared, unique}}, "b": {Blobs: []types.BlobInfo{shared, sig}}},
			archived: sets.New(shared.Digest.String()), metadata: sizeInventory{Bytes: 142, Files: 2},
			wantBytes: 500 + 142 + 4*3072 + 1024},
		{name: "unknown negative size", images: []v2alpha1.CopyImageSchema{a},
			results:   map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{{Digest: shared.Digest, Size: -1}}}},
			wantBytes: 1024, wantErrors: 1},
		{name: "invalid digest", images: []v2alpha1.CopyImageSchema{a},
			results:   map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{{Digest: "invalid", Size: 100}}}},
			wantBytes: 1024, wantErrors: 1},
		{name: "missing source despite unrelated result", images: []v2alpha1.CopyImageSchema{a},
			results: map[string]imageSizeResult{"b": {Blobs: []types.BlobInfo{unique}}}, wantBytes: 1024, wantErrors: 1},
		{name: "source errors once with retained image content", images: []v2alpha1.CopyImageSchema{a, a},
			results:   map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{shared}, Errors: []error{errors.New("signature unavailable")}}},
			wantBytes: 100 + 3072 + 1024, wantErrors: 1},
		{name: "payload overflow", images: []v2alpha1.CopyImageSchema{a},
			results:   map[string]imageSizeResult{"a": {Blobs: []types.BlobInfo{{Digest: shared.Digest, Size: math.MaxInt64}}}},
			wantBytes: math.MaxInt64},
		{name: "metadata overflow", metadata: sizeInventory{Bytes: math.MaxInt64, Files: 1}, wantBytes: math.MaxInt64},
		{name: "entry allowance overflow", metadata: sizeInventory{Files: math.MaxInt64}, wantBytes: math.MaxInt64},
		{name: "canceled empty selection", canceled: true, wantBytes: 1024, wantErrors: 1, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			got := roughTarballSize(ctx, tc.images, tc.results, tc.archived, tc.metadata)
			require.Equal(t, tc.wantBytes, got.Bytes)
			require.Len(t, got.Errors, tc.wantErrors)
			require.Equal(t, tc.unavailable, got.Unavailable)
			if tc.canceled {
				require.ErrorIs(t, got.Errors[0], context.Canceled)
			}
		})
	}
}

func TestRoughTarballMetadataAndOmissions(t *testing.T) {
	ex := newIncrementalExecutor(t, mirror.MirrorToDisk)
	blob := types.BlobInfo{Digest: digest.FromString("cached"), Size: 8192}
	img := v2alpha1.CopyImageSchema{Source: "docker://source.example/image:latest", Destination: "docker://localhost:55000/image:latest"}
	results := map[string]imageSizeResult{img.Source: {Blobs: []types.BlobInfo{blob}}}
	images := []v2alpha1.CopyImageSchema{img}
	metadata, err := archive.CacheMetadata(images, map[string][]types.BlobInfo{img.Source: {blob}})
	require.NoError(t, err)
	write := func(path string, size int64) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		require.NoError(t, os.WriteFile(path, make([]byte, size), 0600))
	}
	path, err := archive.BlobPath(ex.LocalStorageDisk, blob.Digest)
	require.NoError(t, err)
	write(path, blob.Size)
	for path, size := range metadata {
		write(filepath.Join(ex.LocalStorageDisk, path), size)
	}
	cs := v2alpha1.CollectorSchema{AllImages: images}
	before := ex.estimateDryRunSize(context.Background(), cs, results)
	require.Zero(t, before.TotalDownloadSize.Bytes)
	require.Zero(t, before.LocalStorageSize.Bytes)
	require.Equal(t, blob.Size+71+2*3072+1024, before.TarballSize.Bytes, "full link length, not incremental zero")

	write(filepath.Join(ex.Opts.Global.WorkingDir, "operator-catalogs", "layout"), 1024*1024)
	// A broken workfile symlink would fail the retired archive walk; it must not
	// be visited by rough sizing. Real archive validation remains separate.
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(ex.Opts.Global.WorkingDir, "broken")))
	write(filepath.Join(ex.LocalStorageDisk, "docker/registry/v2/repositories/unrelated/link"), 1024*1024)
	write(ex.Opts.Global.ConfigPath, 1024*1024)
	after := ex.estimateDryRunSize(context.Background(), cs, results)
	require.Equal(t, before, after, "workfiles, ISC and unrelated cache metadata must not affect the rough inventory")
	ex.logSizeReport(after)
	out := ex.Log.(*capturingLogger).output()
	require.Contains(t, out, "Tarball Size is rough")
	require.Contains(t, out, "excludes working-dir (including catalog layouts)")
	require.Contains(t, out, "unrelated cached metadata")
	require.Contains(t, out, "Not a peak-disk guarantee")

	for path := range metadata {
		write(filepath.Join(ex.LocalStorageDisk, path), 10)
	}
	after = ex.estimateDryRunSize(context.Background(), cs, results)
	require.Equal(t, int64(61), after.LocalStorageSize.Bytes, "only missing link growth is incremental")
	require.Equal(t, before.TarballSize, after.TarballSize)
}

func TestRoughTarballHistoryErrorsAndReadOnly(t *testing.T) {
	for _, tc := range []string{"initialize failure", "read failure", "canceled", "read only"} {
		t.Run(tc, func(t *testing.T) {
			ex := newIncrementalExecutor(t, mirror.MirrorToDisk)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch tc {
			case "initialize failure":
				ex.Opts.Global.WorkingDir = ex.Opts.Global.ConfigPath
			case "read failure":
				dir := filepath.Join(ex.Opts.Global.WorkingDir, ".history")
				require.NoError(t, os.MkdirAll(dir, 0755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".history-bad-date"), nil, 0600))
			case "canceled":
				cancel()
			case "read only":
				h, err := history.NewHistory(ex.Opts.Global.WorkingDir, time.Time{}, ex.Log, history.OSFileCreator{})
				require.NoError(t, err)
				_, err = h.Append(sets.New(digest.FromString("old").String()))
				require.NoError(t, err)
			}
			report := ex.estimateDryRunSize(ctx, v2alpha1.CollectorSchema{}, nil)
			if tc != "read only" {
				require.True(t, report.TarballSize.Unavailable)
				require.NotEmpty(t, report.TarballSize.Errors)
				return
			}
			require.Empty(t, report.TarballSize.Errors)
			entries, err := os.ReadDir(filepath.Join(ex.Opts.Global.WorkingDir, ".history"))
			require.NoError(t, err)
			require.Len(t, entries, 1, "sizing must not append history")
		})
	}
}

func TestRoughTarballSignatureLinks(t *testing.T) {
	ex := newIncrementalExecutor(t, mirror.MirrorToDisk)
	top := types.BlobInfo{Digest: digest.FromString("manifest"), Size: 300, MediaType: "application/vnd.oci.image.manifest.v1+json"}
	sig := types.BlobInfo{Digest: digest.FromString("signature"), Size: 400, MediaType: top.MediaType,
		Annotations: map[string]string{consts.SignatureFor: top.Digest.String()}}
	img := v2alpha1.CopyImageSchema{Source: "docker://source.example/image:latest", Destination: "docker://localhost:55000/image:latest"}
	cs := v2alpha1.CollectorSchema{AllImages: []v2alpha1.CopyImageSchema{img, img}}
	report := ex.estimateDryRunSize(context.Background(), cs, map[string]imageSizeResult{img.Source: {Blobs: []types.BlobInfo{sig, top}}})
	require.Empty(t, report.TarballSize.Errors)
	require.Equal(t, int64(700+6*71+8*3072+1024), report.TarballSize.Bytes, "two blobs, six full signature/revision/tag links")
}

func TestRoughTarballMetadataFailure(t *testing.T) {
	ex := newIncrementalExecutor(t, mirror.MirrorToDisk)
	img := v2alpha1.CopyImageSchema{Source: "docker://source.example/image:latest", Destination: ""}
	report := ex.estimateDryRunSize(context.Background(), v2alpha1.CollectorSchema{AllImages: []v2alpha1.CopyImageSchema{img}},
		map[string]imageSizeResult{img.Source: {}})
	require.NotEmpty(t, report.LocalStorageSize.Errors)
	require.True(t, report.TarballSize.Unavailable, "invalid selected link prediction must not look like a complete zero")
	require.NotEmpty(t, report.TarballSize.Errors)
}
