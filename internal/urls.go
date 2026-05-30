package internal

// urls.go is the single source of truth for every upstream URL used by the
// build and version-discovery pipeline.  Changing an upstream URL scheme
// requires editing only this file.

const (
	// ── Kubernetes ────────────────────────────────────────────────────────────
	// Release schedule YAML used to discover active minor + patch versions.
	urlK8sSchedule = "https://raw.githubusercontent.com/kubernetes/website/main/data/releases/schedule.yaml"

	// Binary download: args are (version, arch, binary)  e.g. "1.33.1", "amd64", "kubelet"
	urlKubernetesBin = "https://dl.k8s.io/v%s/bin/linux/%s/%s"

	// ── EKS-D ─────────────────────────────────────────────────────────────────
	// GitHub releases API for version discovery.
	urlEKSDReleasesAPI = "https://api.github.com/repos/aws/eks-distro/releases"

	// Release manifest: args are (minor, minor, releaseN)  e.g. "1-35", "1-35", "9"
	urlEKSDManifest = "https://distro.eks.amazonaws.com/kubernetes-%s/kubernetes-%s-eks-%s.yaml"

	// Binary download: args are (minor, releaseN, k8sVersion, arch, binary)
	urlEKSDBin = "https://distro.eks.amazonaws.com/kubernetes-%s/releases/%s/artifacts/kubernetes/%s/bin/linux/%s"

	// ── containerd ────────────────────────────────────────────────────────────
	// GitHub releases API for version discovery.
	urlContainerdReleasesAPI = "https://api.github.com/repos/containerd/containerd/releases"

	// Static tarball: args are (version, version, arch)  e.g. "2.3.1", "2.3.1", "amd64"
	urlContainerdTar = "https://github.com/containerd/containerd/releases/download/v%s/containerd-static-%s-linux-%s.tar.gz"

	// runc version pin file inside the containerd source tree: arg is (version)
	urlContainerdRuncVersion = "https://raw.githubusercontent.com/containerd/containerd/refs/tags/v%s/script/setup/runc-version"

	// ── runc ──────────────────────────────────────────────────────────────────
	// Binary: args are (version, arch)  e.g. "v1.4.2", "amd64"
	urlRuncBin = "https://github.com/opencontainers/runc/releases/download/%s/runc.%s"

	// SHA-256 checksum file: arg is (version)
	urlRuncSHA256 = "https://github.com/opencontainers/runc/releases/download/%s/runc.sha256sum"

	// ── CRI-O ─────────────────────────────────────────────────────────────────
	// GitHub releases API for version discovery.
	urlCRIOReleasesAPI = "https://api.github.com/repos/cri-o/cri-o/releases"

	// Static tarball on GCS: args are (arch, version)  e.g. "amd64", "1.35.3"
	urlCRIOTar = "https://storage.googleapis.com/cri-o/artifacts/cri-o.%s.v%s.tar.gz"

	// ── k3s ───────────────────────────────────────────────────────────────────
	// GitHub releases API for version discovery.
	urlK3sReleasesAPI = "https://api.github.com/repos/k3s-io/k3s/releases"

	// Binary: args are (version, binary)  e.g. "1.36.1%2Bk3s1", "k3s"
	urlK3sBin = "https://github.com/k3s-io/k3s/releases/download/v%s/%s"

	// ── CNI plugins ───────────────────────────────────────────────────────────
	// GitHub releases API for version discovery.
	urlCNIReleasesAPI = "https://api.github.com/repos/containernetworking/plugins/releases"

	// Tarball: args are (version, arch, version)  e.g. "1.9.1", "amd64", "1.9.1"
	urlCNITar = "https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz"

	// SHA-256 checksum: same args as urlCNITar
	urlCNISHA256 = "https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz.sha256"

	// ── cri-tools (crictl) ────────────────────────────────────────────────────
	// GitHub releases API used to resolve crictl version from k8s minor.
	urlCRIToolsReleasesAPI = "https://api.github.com/repos/kubernetes-sigs/cri-tools/releases"

	// Tarball: args are (version, version, arch)  e.g. "1.33.0", "1.33.0", "amd64"
	urlCrictlTar = "https://github.com/kubernetes-sigs/cri-tools/releases/download/v%s/crictl-v%s-linux-%s.tar.gz"
)
