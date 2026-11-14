/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestExportMissingRoot(t *testing.T) {
	for _, mediaType := range []string{ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex} {
		for _, multiple := range []bool{false, true} {
			name := mediaType + "/single"
			if multiple {
				name = mediaType + "/multiple"
			}
			t.Run(name, func(t *testing.T) {
				store, err := local.NewStore(t.TempDir())
				require.NoError(t, err)
				missing := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromString("missing root"), Size: 123}
				imgs := []images.Image{{Name: "example.test/missing:latest", Target: missing}}
				if multiple {
					imgs = append([]images.Image{{Name: "example.test/present:latest", Target: writeExportImage(t, store)}}, imgs...)
				}
				var out bytes.Buffer
				err = Export(t.Context(), store, &out, WithImages(imgs))
				require.ErrorIs(t, err, errdefs.ErrNotFound)
			})
		}
	}
}

func TestExportRootMetadataError(t *testing.T) {
	for _, infoErr := range []error{errors.New("metadata unavailable"), context.Canceled, context.DeadlineExceeded} {
		t.Run(infoErr.Error(), func(t *testing.T) {
			store, err := local.NewStore(t.TempDir())
			require.NoError(t, err)
			root := writeExportImage(t, store)
			provider := exportInfoProvider{InfoReaderProvider: store, digest: root.Digest, err: infoErr}
			var out bytes.Buffer
			err = Export(t.Context(), provider, &out, WithManifest(root))
			require.ErrorIs(t, err, infoErr)
			require.ErrorContains(t, err, root.Digest.String())
		})
	}
}

func TestExportMissingRootMetadata(t *testing.T) {
	store, err := local.NewStore(t.TempDir())
	require.NoError(t, err)
	root := writeExportImage(t, store)
	provider := exportInfoProvider{InfoReaderProvider: store, digest: root.Digest, err: errdefs.ErrNotFound}
	var out bytes.Buffer
	require.NoError(t, Export(t.Context(), provider, &out, WithManifest(root)))
	entries := readExportEntries(t, out.Bytes())
	var index ocispec.Index
	require.NoError(t, json.Unmarshal(entries[ocispec.ImageIndexFile], &index))
	require.Len(t, index.Manifests, 1)
	require.Equal(t, root.Digest, index.Manifests[0].Digest)
	require.Contains(t, entries, "blobs/sha256/"+root.Digest.Encoded())
	require.Contains(t, entries, "manifest.json")
}

func TestExportRootAnnotations(t *testing.T) {
	for _, sourceLabels := range []map[string]string{nil, {
		labels.LabelDistributionSource + ".example.test": "library/test",
		"unrelated": "ignored",
	}} {
		name := "without source labels"
		if sourceLabels != nil {
			name = "with source labels"
		}
		t.Run(name, func(t *testing.T) {
			store, err := local.NewStore(t.TempDir())
			require.NoError(t, err)
			root := writeExportImage(t, store)
			root.Annotations = map[string]string{"example.test/annotation": "preserved"}
			info, err := store.Info(t.Context(), root.Digest)
			require.NoError(t, err)
			info.Labels = sourceLabels
			provider := exportInfoProvider{InfoReaderProvider: store, digest: root.Digest, info: info}
			var out bytes.Buffer
			require.NoError(t, Export(t.Context(), provider, &out, WithManifest(root, "example.test/test:latest")))
			entries := readExportEntries(t, out.Bytes())
			var index ocispec.Index
			require.NoError(t, json.Unmarshal(entries[ocispec.ImageIndexFile], &index))
			require.Len(t, index.Manifests, 1)
			annotations := map[string]string{
				"example.test/annotation":  "preserved",
				images.AnnotationImageName: "example.test/test:latest",
				ocispec.AnnotationRefName:  "latest",
			}
			if sourceLabels != nil {
				annotations[labels.LabelDistributionSource+".example.test"] = "library/test"
			}
			require.Equal(t, root.Digest, index.Manifests[0].Digest)
			require.Equal(t, annotations, index.Manifests[0].Annotations)
		})
	}
}

func TestExportSkipMissingRoot(t *testing.T) {
	for _, mediaType := range []string{ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex} {
		t.Run(mediaType, func(t *testing.T) {
			for _, skipDocker := range []bool{false, true} {
				name := "with Docker manifest"
				if skipDocker {
					name = "without Docker manifest"
				}
				t.Run(name, func(t *testing.T) {
					store, err := local.NewStore(t.TempDir())
					require.NoError(t, err)
					root := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromString("missing root"), Size: 123}
					opts := []ExportOpt{WithManifest(root), WithSkipMissing(store)}
					if skipDocker {
						opts = append(opts, WithSkipDockerManifest())
					}
					var out bytes.Buffer
					err = Export(t.Context(), store, &out, opts...)
					if mediaType == ocispec.MediaTypeImageIndex || !skipDocker {
						require.ErrorIs(t, err, errdefs.ErrNotFound)
						return
					}
					require.NoError(t, err)
					entries := readExportEntries(t, out.Bytes())
					var index ocispec.Index
					require.NoError(t, json.Unmarshal(entries[ocispec.ImageIndexFile], &index))
					require.Len(t, index.Manifests, 1)
					require.Equal(t, root.Digest, index.Manifests[0].Digest)
					require.NotContains(t, entries, "blobs/sha256/"+root.Digest.Encoded())
				})
			}
		})
	}
}

func TestExportSkipMissingPlatform(t *testing.T) {
	store, err := local.NewStore(t.TempDir())
	require.NoError(t, err)
	present := writeExportImage(t, store)
	missing := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("missing platform"), Size: 123}
	root := writeExportBlob(t, store, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{present, missing},
	})
	for _, skip := range []bool{false, true} {
		name := "require all content"
		if skip {
			name = "skip missing content"
		}
		t.Run(name, func(t *testing.T) {
			opts := []ExportOpt{WithManifest(root), WithAllPlatforms(), WithPlatform(platforms.All)}
			if skip {
				opts = append(opts, WithSkipMissing(store))
			}
			var out bytes.Buffer
			err := Export(t.Context(), store, &out, opts...)
			if !skip {
				require.ErrorIs(t, err, errdefs.ErrNotFound)
				return
			}
			require.NoError(t, err)
			entries := readExportEntries(t, out.Bytes())
			var index ocispec.Index
			require.NoError(t, json.Unmarshal(entries[ocispec.ImageIndexFile], &index))
			require.Len(t, index.Manifests, 1)
			require.Equal(t, root.Digest, index.Manifests[0].Digest)
			require.Contains(t, entries, "blobs/sha256/"+present.Digest.Encoded())
			require.NotContains(t, entries, "blobs/sha256/"+missing.Digest.Encoded())
			require.Contains(t, entries, "manifest.json")
		})
	}
}

type exportInfoProvider struct {
	content.InfoReaderProvider
	digest digest.Digest
	info   content.Info
	err    error
}

func (p exportInfoProvider) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	if dgst == p.digest {
		return p.info, p.err
	}
	return p.InfoReaderProvider.Info(ctx, dgst)
}

func writeExportImage(t *testing.T, store content.Store) ocispec.Descriptor {
	t.Helper()
	config := writeExportBlob(t, store, ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: ocispec.Platform{Architecture: "arm64", OS: "linux"},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{}},
	})
	return writeExportBlob(t, store, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{},
	})
}

func writeExportBlob(t *testing.T, store content.Store, mediaType string, value any) ocispec.Descriptor {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
	require.NoError(t, content.WriteBlob(t.Context(), store, desc.Digest.String(), bytes.NewReader(b), desc))
	return desc
}

func readExportEntries(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	entries := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		require.NoError(t, err)
		entries[hdr.Name], err = io.ReadAll(tr)
		require.NoError(t, err)
	}
}
