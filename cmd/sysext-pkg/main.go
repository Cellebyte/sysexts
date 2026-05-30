package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cellebyte/k8s-sysext/internal"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// ── global flags ──────────────────────────────────────────────────────────────

type globalFlags struct {
	registry   string
	repository string
}

func (g *globalFlags) ociConfig() internal.OCIConfig {
	reg := g.registry
	if reg == "" {
		reg = os.Getenv("OCI_REGISTRY")
	}
	return internal.OCIConfig{
		Registry:   reg,
		Repository: g.repository,
	}
}

// ── root ──────────────────────────────────────────────────────────────────────

func rootCmd() *cobra.Command {
	gf := &globalFlags{}

	root := &cobra.Command{
		Use:   "sysext-pkg",
		Short: "Build and publish systemd-sysext images for Kubernetes node components",
	}

	root.PersistentFlags().StringVar(&gf.registry, "registry", "",
		`OCI registry hostname (default "ghcr.io"; overrides OCI_REGISTRY env)`)
	root.PersistentFlags().StringVar(&gf.repository, "repository", "cellebyte/sysexts",
		"OCI repository path (e.g. cellebyte/sysexts)")

	root.AddCommand(
		listCmd(),
		buildCmd(gf),
		pushCmd(gf),
		releaseCmd(gf),
	)
	return root
}

// ── list ──────────────────────────────────────────────────────────────────────

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [component]",
		Short: "List available upstream versions",
		Long: `List available upstream versions for one component or all components.

Without an argument, all components are listed with lines like "component:version".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) == 1 {
				return listOne(ctx, args[0])
			}
			for name, comp := range internal.Registry {
				versions, err := comp.ListVersions(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warn: %s: %v\n", name, err)
					continue
				}
				for _, v := range versions {
					fmt.Printf("%s:%s\n", name, v)
				}
			}
			return nil
		},
	}
}

func listOne(ctx context.Context, name string) error {
	comp, err := internal.Get(name)
	if err != nil {
		return err
	}
	versions, err := comp.ListVersions(ctx)
	if err != nil {
		return err
	}
	for _, v := range versions {
		fmt.Println(v)
	}
	return nil
}

// ── build ─────────────────────────────────────────────────────────────────────

func buildCmd(gf *globalFlags) *cobra.Command {
	var (
		outDir      string
		staticFiles string
	)

	_ = gf // build doesn't need OCI config; kept for signature symmetry

	cmd := &cobra.Command{
		Use:   "build <component> <version> <arch>",
		Short: "Download binaries and build a .raw squashfs sysext image",
		Long: `Download all binaries for the given component + version + arch, overlay any
static files (systemd units, configs), and pack everything into a squashfs
.raw file ready for systemd-sysext.

arch must be "amd64" or "arm64".`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd.Context(), args[0], args[1], args[2], outDir, staticFiles)
		},
	}

	cmd.Flags().StringVar(&outDir, "out", ".", "directory where the .raw file is written")
	cmd.Flags().StringVar(&staticFiles, "static-files", "",
		"override path to static files directory (default: <component>.sysext/files)")
	return cmd
}

func runBuild(ctx context.Context, component, version, arch, outDir, staticFilesOverride string) error {
	comp, err := internal.Get(component)
	if err != nil {
		return err
	}

	stagingDir, err := os.MkdirTemp("", "k8s-sysext-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	fmt.Printf("fetching %s %s (%s)…\n", component, version, arch)
	if err := comp.Fetch(ctx, version, arch, stagingDir); err != nil {
		return fmt.Errorf("fetch %s: %w", component, err)
	}

	// Overlay static files (systemd units, configs) on top of staged binaries.
	staticDir := staticFilesOverride
	if staticDir == "" {
		staticDir = comp.StaticFilesDir
	}
	if staticDir != "" {
		if fi, serr := os.Stat(staticDir); serr == nil && fi.IsDir() {
			if err := mergeDir(staticDir, stagingDir); err != nil {
				return fmt.Errorf("merge static files: %w", err)
			}
		}
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	outFile := filepath.Join(outDir, internal.OutputFileName(component, version, arch))

	// The sysext name written into extension-release.d must exactly match the
	// filename that systemd-sysext sees on disk (minus the .raw suffix).
	sysextName := strings.TrimSuffix(filepath.Base(outFile), ".raw")

	fmt.Printf("building %s…\n", outFile)
	if err := internal.CreateSysext(stagingDir, outFile, sysextName, arch); err != nil {
		return fmt.Errorf("create sysext: %w", err)
	}

	fmt.Printf("wrote %s\n", outFile)
	return nil
}

// mergeDir copies the tree at src on top of dst, overwriting any existing
// files.  Used to layer systemd units and configs over staged binaries.
func mergeDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		destPath := filepath.Join(dst, rel)

		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(destPath)
			return os.Symlink(target, destPath)
		}
		if d.IsDir() {
			return os.MkdirAll(destPath, info.Mode().Perm())
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		return copyFile(path, destPath, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// ── push ──────────────────────────────────────────────────────────────────────

func pushCmd(gf *globalFlags) *cobra.Command {
	var rawFile string

	cmd := &cobra.Command{
		Use:   "push <component> <version> <arch>",
		Short: "Push a pre-built .raw sysext image to the OCI registry",
		Long: `Push a single-arch .raw sysext image to the OCI registry, tagged
"<version>-<arch>" (e.g. "v1.33.1-amd64").

Use "release" to build both arches and publish the multi-arch index.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			component, version, arch := args[0], args[1], args[2]
			if rawFile == "" {
				rawFile = internal.OutputFileName(component, version, arch)
			}
			cfg := gf.ociConfig()
			ctx := cmd.Context()

			fmt.Printf("pushing %s to %s/%s/%s…\n",
				rawFile, cfg.Registry, cfg.Repository, component)
			desc, err := internal.PushVariant(ctx, cfg, rawFile, component, version, arch)
			if err != nil {
				return err
			}
			fmt.Printf("pushed %s@%s\n", component, desc.Digest)
			return nil
		},
	}

	cmd.Flags().StringVar(&rawFile, "file", "",
		"path to .raw file (default: <component>-<version>-<arch>.raw)")
	return cmd
}

// ── release ───────────────────────────────────────────────────────────────────

func releaseCmd(gf *globalFlags) *cobra.Command {
	var (
		arches       []string
		outDir       string
		staticFiles  string
		skipBuild    bool
		skipExisting bool
	)

	cmd := &cobra.Command{
		Use:   "release <component> <version>",
		Short: "Build, push variants, and publish the multi-arch OCI index",
		Long: `Full release pipeline for a single component + version:

  1. Build the .raw squashfs for each arch (skipped with --skip-build)
  2. Push each arch variant tagged "<version>-<arch>"
  3. Push an OCI Image Index tagged "<version>", "<minor>", and "latest"

Pass --skip-existing to skip the release if the version tag already exists.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			component, version := args[0], args[1]
			cfg := gf.ociConfig()
			ctx := cmd.Context()

			if skipExisting {
				exists, err := internal.TagExists(ctx, cfg, component, version)
				if err != nil {
					return fmt.Errorf("check tag: %w", err)
				}
				if exists {
					fmt.Printf("%s:%s already published, skipping\n", component, version)
					return nil
				}
			}

			var descs []ocispec.Descriptor
			for _, arch := range arches {
				if !skipBuild {
					if err := runBuild(ctx, component, version, arch, outDir, staticFiles); err != nil {
						return err
					}
				}

				rawFile := filepath.Join(outDir, internal.OutputFileName(component, version, arch))
				fmt.Printf("pushing %s (%s)…\n", component, arch)
				desc, err := internal.PushVariant(ctx, cfg, rawFile, component, version, arch)
				if err != nil {
					return fmt.Errorf("push %s/%s: %w", component, arch, err)
				}
				descs = append(descs, desc)
			}

			tags := internal.IndexTags(version)
			fmt.Printf("publishing index %s → %s\n", component, strings.Join(tags, ", "))
			if err := internal.PushIndex(ctx, cfg, component, descs, tags); err != nil {
				return fmt.Errorf("push index: %w", err)
			}

			fmt.Printf("released %s %s\n", component, version)
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&arches, "arches", []string{"amd64", "arm64"},
		"architectures to build and push")
	cmd.Flags().StringVar(&outDir, "out", ".", "directory for .raw files")
	cmd.Flags().StringVar(&staticFiles, "static-files", "",
		"override path to static files directory")
	cmd.Flags().BoolVar(&skipBuild, "skip-build", false,
		"skip the build step (assumes .raw files already exist in --out)")
	cmd.Flags().BoolVar(&skipExisting, "skip-existing", false,
		"skip if the version tag already exists in the registry")
	return cmd
}
