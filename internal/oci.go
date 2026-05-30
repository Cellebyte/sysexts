package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// artifactType identifies sysext images in OCI registries.
	artifactType = "application/vnd.systemd.sysext.v1"

	// layerMediaType is the media type for the .raw squashfs layer.
	layerMediaType = "application/vnd.systemd.sysext.image.v1+raw"
)

// OCIConfig holds registry credentials and repository coordinates.
type OCIConfig struct {
	// Registry is the hostname, e.g. "ghcr.io".
	// Defaults to "ghcr.io" if empty.
	Registry string

	// Repository is the image repository path, e.g. "cellebyte/sysexts".
	Repository string

	// Token is used as the registry password (Bearer token / PAT).
	// Reads GITHUB_TOKEN from environment if empty.
	Token string
}

func (c *OCIConfig) registry() string {
	if c.Registry == "" {
		return "ghcr.io"
	}
	return c.Registry
}

func (c *OCIConfig) token() string {
	if c.Token != "" {
		return c.Token
	}
	return os.Getenv("GITHUB_TOKEN")
}

// newRepo returns an authenticated remote.Repository for the given component.
func (c *OCIConfig) newRepo(component string) (*remote.Repository, error) {
	ref := fmt.Sprintf("%s/%s/%s", c.registry(), c.Repository, component)
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, err
	}
	repo.Client = &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.NewCache(),
		Credential: auth.StaticCredential(c.registry(), auth.Credential{
			Username: "token",
			Password: c.token(),
		}),
	}
	return repo, nil
}

// PushVariant pushes a single-arch .raw sysext image to the OCI registry and
// returns the manifest descriptor needed to build the multi-arch index.
//
// The image is tagged "<version>-<arch>", e.g. "v1.33.1-amd64".
func PushVariant(ctx context.Context, cfg OCIConfig, rawFile, component, version, arch string) (ocispec.Descriptor, error) {
	variantTag := fmt.Sprintf("%s-%s", version, arch)

	store, err := file.New("")
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("file store: %w", err)
	}
	defer store.Close()

	// Register the .raw file as a single layer.
	layerDesc, err := store.Add(ctx, rawFile, layerMediaType, rawFile)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("add layer: %w", err)
	}

	// Pack an OCI manifest (spec v1.1 artifact).
	manifestDesc, err := oras.PackManifest(ctx, store,
		oras.PackManifestVersion1_1,
		artifactType,
		oras.PackManifestOptions{
			Layers: []ocispec.Descriptor{layerDesc},
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationVersion: version,
			},
		},
	)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack manifest: %w", err)
	}

	if err := store.Tag(ctx, manifestDesc, variantTag); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("tag manifest: %w", err)
	}

	repo, err := cfg.newRepo(component)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	if _, err := oras.Copy(ctx, store, variantTag, repo, variantTag, oras.DefaultCopyOptions); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("push %s: %w", variantTag, err)
	}

	// Attach platform info for the multi-arch index.
	manifestDesc.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
	return manifestDesc, nil
}

// PushIndex creates an OCI Image Index pointing to the per-arch manifests and
// pushes it under every tag in tags.
//
// descriptors are the values returned by PushVariant for each arch.
// tags is typically []string{"v1.33.1", "v1.33"}.
func PushIndex(ctx context.Context, cfg OCIConfig, component string, descriptors []ocispec.Descriptor, tags []string) error {
	if len(descriptors) == 0 {
		return fmt.Errorf("no arch descriptors provided")
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: descriptors,
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	indexDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.FromBytes(indexJSON),
		Size:      int64(len(indexJSON)),
	}

	repo, err := cfg.newRepo(component)
	if err != nil {
		return err
	}

	for _, tag := range tags {
		if err := repo.PushReference(ctx, indexDesc, bytes.NewReader(indexJSON), tag); err != nil {
			return fmt.Errorf("push index tag %q: %w", tag, err)
		}
	}
	return nil
}

// TagExists reports whether the given tag already exists in the registry.
// A missing tag (HTTP 404) returns (false, nil).
func TagExists(ctx context.Context, cfg OCIConfig, component, tag string) (bool, error) {
	repo, err := cfg.newRepo(component)
	if err != nil {
		return false, err
	}
	_, err = repo.Resolve(ctx, tag)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// IndexTags returns all OCI tags to push for a given version:
// the immutable full version plus the mutable minor tag.
//
//	IndexTags("v1.33.1") → ["v1.33.1", "v1.33"]
func IndexTags(version string) []string {
	minor := k8sMajorMinor(version) // "v1.33"
	return []string{version, minor}
}

// isNotFound reports whether err represents an HTTP 404 / not-found response.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}
