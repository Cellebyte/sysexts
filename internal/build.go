package internal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---- Kubernetes / EKS-D -------------------------------------------------

// FetchKubernetes downloads kubelet, kubeadm, kubectl and crictl into
// stagingDir, mirroring the final /usr/bin/ layout.
func FetchKubernetes(ctx context.Context, version, arch, stagingDir string) error {
	relArch := toReleaseArch(arch) // x86-64 → amd64
	binDir := filepath.Join(stagingDir, "usr", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}

	// kubelet, kubeadm, kubectl
	for _, bin := range []string{"kubelet", "kubeadm", "kubectl"} {
		url := fmt.Sprintf("https://dl.k8s.io/%s/bin/linux/%s/%s", version, relArch, bin)
		dest := filepath.Join(binDir, bin)
		if err := downloadFile(ctx, url, dest, "", 0755); err != nil {
			return fmt.Errorf("download %s: %w", bin, err)
		}
	}

	// crictl — version tracks k8s minor (e.g. v1.33.x → cri-tools v1.33.x)
	critctlVersion, err := latestCRIToolsForK8s(ctx, version)
	if err != nil {
		return fmt.Errorf("resolve crictl version: %w", err)
	}
	if err := fetchCrictl(ctx, critctlVersion, relArch, binDir); err != nil {
		return fmt.Errorf("fetch crictl: %w", err)
	}

	// Version marker used by the kubelet drop-in.
	shareDir := filepath.Join(stagingDir, "usr", "local", "share")
	if err := os.MkdirAll(shareDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(shareDir, "kubernetes-version"), []byte(version), 0644)
}

// FetchEKSD downloads EKS-D kubelet, kubeadm, kubectl and crictl.
// version is the EKS-D release tag, e.g. "v1-35-eks-9".
func FetchEKSD(ctx context.Context, version, arch, stagingDir string) error {
	relArch := toReleaseArch(arch)
	binDir := filepath.Join(stagingDir, "usr", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}

	// Parse "v1-35-eks-9" → minor "1-35", release "9"
	minor, releaseN, err := parseEKSDTag(version)
	if err != nil {
		return err
	}

	// Fetch the release manifest to get the exact k8s semver (e.g. "v1.35.4").
	k8sVersion, err := fetchEKSDKubeVersion(ctx, minor, releaseN)
	if err != nil {
		return fmt.Errorf("resolve eks-d k8s version: %w", err)
	}

	baseURL := fmt.Sprintf(
		"https://distro.eks.amazonaws.com/kubernetes-%s/releases/%s/artifacts/kubernetes/%s/bin/linux/%s",
		minor, releaseN, k8sVersion, relArch,
	)

	for _, bin := range []string{"kubelet", "kubeadm", "kubectl"} {
		url := baseURL + "/" + bin
		dest := filepath.Join(binDir, bin)
		if err := downloadFile(ctx, url, dest, "", 0755); err != nil {
			return fmt.Errorf("download eks-d %s: %w", bin, err)
		}
	}

	// crictl — version tracks k8s minor
	critctlVersion, err := latestCRIToolsForK8s(ctx, k8sVersion)
	if err != nil {
		return fmt.Errorf("resolve crictl version: %w", err)
	}
	if err := fetchCrictl(ctx, critctlVersion, relArch, binDir); err != nil {
		return fmt.Errorf("fetch crictl: %w", err)
	}

	shareDir := filepath.Join(stagingDir, "usr", "local", "share")
	if err := os.MkdirAll(shareDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(shareDir, "kubernetes-version"), []byte(version), 0644)
}

// ---- containerd ---------------------------------------------------------

// FetchContainerd downloads the static containerd tarball and the runc
// binary pinned by that containerd release.
func FetchContainerd(ctx context.Context, version, arch, stagingDir string) error {
	relArch := toReleaseArch(arch)
	binDir := filepath.Join(stagingDir, "usr", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}

	// containerd static tarball
	tarURL := fmt.Sprintf(
		"https://github.com/containerd/containerd/releases/download/v%s/containerd-static-%s-linux-%s.tar.gz",
		version, version, relArch,
	)
	sha256URL := tarURL + ".sha256sum"

	expectedHash, err := fetchTextContent(ctx, sha256URL)
	if err != nil {
		return fmt.Errorf("fetch containerd sha256: %w", err)
	}
	// sha256sum file format: "<hash>  <filename>"
	expectedHash = strings.Fields(expectedHash)[0]

	tmpTar := filepath.Join(stagingDir, "containerd.tar.gz")
	if err := downloadFile(ctx, tarURL, tmpTar, expectedHash, 0644); err != nil {
		return fmt.Errorf("download containerd: %w", err)
	}
	if err := extractTar(tmpTar, binDir, "bin/"); err != nil {
		return fmt.Errorf("extract containerd: %w", err)
	}
	os.Remove(tmpTar)

	// runc — version pinned inside the containerd source tree
	runcVersionURL := fmt.Sprintf(
		"https://raw.githubusercontent.com/containerd/containerd/refs/tags/v%s/script/setup/runc-version",
		version,
	)
	runcVersion, err := fetchTextContent(ctx, runcVersionURL)
	if err != nil {
		return fmt.Errorf("fetch runc version pin: %w", err)
	}
	runcVersion = strings.TrimSpace(runcVersion)

	runcURL := fmt.Sprintf(
		"https://github.com/opencontainers/runc/releases/download/%s/runc.%s",
		runcVersion, relArch,
	)
	runcSHA256URL := fmt.Sprintf(
		"https://github.com/opencontainers/runc/releases/download/%s/runc.sha256sum",
		runcVersion,
	)
	runcHashFile, err := fetchTextContent(ctx, runcSHA256URL)
	if err != nil {
		return fmt.Errorf("fetch runc sha256: %w", err)
	}
	runcHash := extractHashForFile(runcHashFile, "runc."+relArch)

	dest := filepath.Join(binDir, "runc")
	if err := downloadFile(ctx, runcURL, dest, runcHash, 0755); err != nil {
		return fmt.Errorf("download runc: %w", err)
	}

	return nil
}

// ---- CRI-O --------------------------------------------------------------

// FetchCRIO downloads the CRI-O static bundle for the given version.
func FetchCRIO(ctx context.Context, version, arch, stagingDir string) error {
	relArch := toReleaseArch(arch)
	// ListCRIOVersions returns tags with a "v" prefix; strip it so the URL
	// does not become "vv1.35.3".
	version = strings.TrimPrefix(version, "v")

	tarURL := fmt.Sprintf(
		"https://storage.googleapis.com/cri-o/artifacts/cri-o.%s.v%s.tar.gz",
		relArch, version,
	)
	sha256URL := tarURL + ".sha256sum"

	expectedHash, err := fetchTextContent(ctx, sha256URL)
	if err == nil {
		expectedHash = strings.Fields(expectedHash)[0]
	}

	tmpTar := filepath.Join(stagingDir, "crio.tar.gz")
	if err := downloadFile(ctx, tarURL, tmpTar, expectedHash, 0644); err != nil {
		return fmt.Errorf("download cri-o: %w", err)
	}

	// The CRI-O bundle extracts to cri-o/ with bin/ and lib/ subdirectories.
	// Do NOT strip the "cri-o/" prefix — we need it so that the subsequent
	// ReadDir of stagingDir/cri-o/bin actually finds the extracted files.
	localBinDir := filepath.Join(stagingDir, "usr", "local", "bin")
	if err := os.MkdirAll(localBinDir, 0755); err != nil {
		return err
	}
	if err := extractTar(tmpTar, stagingDir, ""); err != nil {
		return fmt.Errorf("extract cri-o: %w", err)
	}
	os.Remove(tmpTar)

	// Move binaries from the extracted cri-o/bin/ to usr/local/bin/
	crioExtracted := filepath.Join(stagingDir, "cri-o", "bin")
	entries, err := os.ReadDir(crioExtracted)
	if err != nil {
		return fmt.Errorf("read cri-o/bin: %w", err)
	}
	for _, e := range entries {
		src := filepath.Join(crioExtracted, e.Name())
		dst := filepath.Join(localBinDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	os.RemoveAll(filepath.Join(stagingDir, "cri-o"))

	return nil
}

// ---- k3s ----------------------------------------------------------------

// FetchK3s downloads the single k3s binary and creates the standard symlinks.
func FetchK3s(ctx context.Context, version, arch, stagingDir string) error {
	relArch := toReleaseArch(arch)

	// k3s uses a different binary name for arm64
	binName := "k3s"
	if relArch == "arm64" {
		binName = "k3s-arm64"
	}

	// k3s version tags use "+" which must be URL-encoded as "%2B"
	encodedVersion := strings.ReplaceAll(version, "+", "%2B")
	url := fmt.Sprintf(
		"https://github.com/k3s-io/k3s/releases/download/%s/%s",
		encodedVersion, binName,
	)

	localBinDir := filepath.Join(stagingDir, "usr", "local", "bin")
	if err := os.MkdirAll(localBinDir, 0755); err != nil {
		return err
	}

	dest := filepath.Join(localBinDir, "k3s")
	if err := downloadFile(ctx, url, dest, "", 0755); err != nil {
		return fmt.Errorf("download k3s: %w", err)
	}

	// Standard symlinks: kubectl, crictl, ctr → k3s
	for _, link := range []string{"kubectl", "crictl", "ctr"} {
		linkPath := filepath.Join(localBinDir, link)
		os.Remove(linkPath) // ignore error if not exists
		if err := os.Symlink("k3s", linkPath); err != nil {
			return fmt.Errorf("symlink %s: %w", link, err)
		}
	}

	return nil
}

// ---- CNI plugins --------------------------------------------------------

// FetchCNIPlugins downloads the standard CNI plugins tarball and places
// binaries at opt/cni/bin/ — which sysext merges directly to /opt/cni/bin/.
func FetchCNIPlugins(ctx context.Context, version, arch, stagingDir string) error {
	relArch := toReleaseArch(arch)
	destDir := filepath.Join(stagingDir, "opt", "cni", "bin")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	url := fmt.Sprintf(
		"https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-%s-%s.tgz",
		version, relArch, version,
	)
	sha256URL := fmt.Sprintf(
		"https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-%s-%s.tgz.sha256",
		version, relArch, version,
	)

	expectedHash, err := fetchTextContent(ctx, sha256URL)
	if err != nil {
		// sha256 file may not exist for all releases; proceed without check
		expectedHash = ""
	}
	expectedHash = strings.TrimSpace(strings.Fields(expectedHash)[0])

	tmpTar := filepath.Join(stagingDir, "cni.tgz")
	if err := downloadFile(ctx, url, tmpTar, expectedHash, 0644); err != nil {
		return fmt.Errorf("download cni-plugins: %w", err)
	}
	// The CNI tarball is a flat directory of binaries.
	if err := extractTar(tmpTar, destDir, ""); err != nil {
		return fmt.Errorf("extract cni-plugins: %w", err)
	}
	os.Remove(tmpTar)

	// Ensure all binaries are executable.
	entries, _ := os.ReadDir(destDir)
	for _, e := range entries {
		if !e.IsDir() {
			os.Chmod(filepath.Join(destDir, e.Name()), 0755)
		}
	}

	return nil
}

// ---- helpers ------------------------------------------------------------

// toReleaseArch converts sysext arch names to release archive arch names.
//
//	"amd64"  → "amd64"  (already correct for most upstreams)
//	"arm64"  → "arm64"
func toReleaseArch(arch string) string {
	// Normalise in case caller passes "x86-64" (legacy sysext convention).
	switch arch {
	case "x86-64", "x86_64":
		return "amd64"
	default:
		return arch
	}
}

// downloadFile fetches url to dest, optionally verifying a SHA-256 hex hash.
// It sets the file's permission bits to perm.
func downloadFile(ctx context.Context, url, dest, expectedSHA256 string, perm os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}

	if expectedSHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if got != expectedSHA256 {
			os.Remove(dest)
			return fmt.Errorf("sha256 mismatch for %s: want %s got %s", url, expectedSHA256, got)
		}
	}

	return nil
}

// fetchTextContent performs a GET and returns the body as a trimmed string.
func fetchTextContent(ctx context.Context, url string) (string, error) {
	data, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// extractTar extracts a .tar.gz archive to destDir.
// If stripPrefix is non-empty, that leading path component is removed from
// each entry path before writing (e.g. "bin/" strips the "bin/" prefix).
func extractTar(tarGz, destDir, stripPrefix string) error {
	f, err := os.Open(tarGz)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		name := hdr.Name
		if stripPrefix != "" {
			if !strings.HasPrefix(name, stripPrefix) {
				continue
			}
			name = strings.TrimPrefix(name, stripPrefix)
		}
		if name == "" || name == "." {
			continue
		}

		dest := filepath.Join(destDir, filepath.Clean(name))

		// Guard against path traversal.
		if !strings.HasPrefix(dest, filepath.Clean(destDir)+string(os.PathSeparator)) &&
			dest != filepath.Clean(destDir) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, os.FileMode(hdr.Mode)|0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			os.Remove(dest)
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return err
			}
		}
	}
	return nil
}

// fetchCrictl downloads the crictl binary for the given version + arch.
func fetchCrictl(ctx context.Context, version, relArch, destDir string) error {
	url := fmt.Sprintf(
		"https://github.com/kubernetes-sigs/cri-tools/releases/download/%s/crictl-%s-linux-%s.tar.gz",
		version, version, relArch,
	)
	tmpTar := filepath.Join(destDir, "crictl.tar.gz")
	if err := downloadFile(ctx, url, tmpTar, "", 0644); err != nil {
		return err
	}
	if err := extractTar(tmpTar, destDir, ""); err != nil {
		return err
	}
	os.Remove(tmpTar)
	os.Chmod(filepath.Join(destDir, "crictl"), 0755)
	return nil
}

// latestCRIToolsForK8s returns the latest cri-tools release whose minor
// version matches the given Kubernetes version (e.g. "v1.33.x" → "v1.33.y").
func latestCRIToolsForK8s(ctx context.Context, k8sVersion string) (string, error) {
	minor := k8sMajorMinor(k8sVersion) // "v1.33"
	tags, err := listGitHubReleases(ctx, "https://api.github.com/repos/kubernetes-sigs/cri-tools/releases")
	if err != nil {
		return "", err
	}
	for _, tag := range tags {
		if strings.HasPrefix(tag, minor+".") {
			return tag, nil
		}
	}
	// Fallback: return the newest available release.
	if len(tags) > 0 {
		return tags[0], nil
	}
	return "", fmt.Errorf("no cri-tools releases found")
}

// k8sMajorMinor returns "v1.33" from "v1.33.1".
func k8sMajorMinor(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return version
	}
	return "v" + parts[0] + "." + parts[1]
}

// parseEKSDTag parses "v1-35-eks-9" into minor="1-35", releaseN="9", error.
func parseEKSDTag(tag string) (minor string, releaseN string, err error) {
	// Expected format: v{MAJOR}-{MINOR}-eks-{N}
	t := strings.TrimPrefix(tag, "v")
	parts := strings.Split(t, "-")
	// parts = ["1", "35", "eks", "9"]
	if len(parts) < 4 || parts[2] != "eks" {
		return "", "", fmt.Errorf("unexpected eks-d tag format: %q", tag)
	}
	minor = parts[0] + "-" + parts[1] // "1-35"
	releaseN = parts[3]                // "9"
	return
}

// fetchEKSDKubeVersion fetches the EKS-D release manifest and extracts the
// full Kubernetes semver (e.g. "v1.35.4") from a kubelet asset URI.
func fetchEKSDKubeVersion(ctx context.Context, minor, releaseN string) (string, error) {
	manifestURL := fmt.Sprintf(
		"https://distro.eks.amazonaws.com/kubernetes-%s/kubernetes-%s-eks-%s.yaml",
		minor, minor, releaseN,
	)
	content, err := fetchTextContent(ctx, manifestURL)
	if err != nil {
		return "", fmt.Errorf("fetch eks-d manifest: %w", err)
	}

	// Scan for a kubelet binary URI line:
	//   uri: https://.../artifacts/kubernetes/v1.35.4/bin/linux/.../kubelet
	const marker = "/artifacts/kubernetes/"
	for _, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, "/bin/linux/") || !strings.Contains(line, "/kubelet") {
			continue
		}
		idx := strings.Index(line, marker)
		if idx < 0 {
			continue
		}
		rest := line[idx+len(marker):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return rest[:slash], nil
		}
	}
	return "", fmt.Errorf("could not determine k8s version from eks-d manifest for %s-eks-%s", minor, releaseN)
}

// extractHashForFile finds the SHA-256 hash for a given filename inside a
// multi-file sha256sum file (format: "<hash>  <filename>\n…").
func extractHashForFile(content, filename string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == filename {
			return fields[0]
		}
	}
	return ""
}
