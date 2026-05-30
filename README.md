# sysext-pkg

A Go CLI that builds [systemd-sysext](https://www.freedesktop.org/software/systemd/man/systemd-sysext.html) images for Kubernetes node components and publishes them as multi-arch OCI artifacts to any OCI registry.

Intended for immutable-OS nodes (Flatcar, FCOS, Talos, etc.) managed by a Cluster API operator.  The operator pulls the appropriate sysext images, drops them into `/var/lib/extensions/`, runs `systemd-sysext merge`, and then handles service activation and restart explicitly — no auto-start magic inside the image.

## Components

| Component | Role | Notes |
|---|---|---|
| `kubernetes` | Kubernetes node | kubelet, kubeadm, kubectl, crictl |
| `eksd` | Kubernetes node | AWS EKS-D drop-in replacement for `kubernetes` |
| `containerd` | Container runtime | containerd v2, runc, ctr, shim; CRI socket `/run/containerd/containerd.sock` |
| `crio` | Container runtime | CRI-O, conmon, pinns, runc, crictl; CRI socket `/var/run/crio/crio.sock` |
| `k3s` | All-in-one | Single binary; bundles node + runtime + CNI — cannot be combined with others |
| `cni-plugins` | CNI | 18 standard plugins at `opt/cni/bin/`; no service units |

### Valid combinations

```
kubernetes  + containerd  [+ cni-plugins]
kubernetes  + crio        [+ cni-plugins]
eksd        + containerd  [+ cni-plugins]
eksd        + crio        [+ cni-plugins]
k3s                        (exclusive — bundles everything)
```

`ValidateCombination` enforces these rules at build time.

## Filesystem layout

Each sysext overlays only `/usr/` and `/opt/`:

```
usr/bin/          kubelet, kubeadm, kubectl, crictl, …
usr/lib/systemd/system/   kubelet.service, containerd.service, …
usr/lib/extension-release.d/extension-release.<name>
usr/etc/containerd/config.toml   (containerd only; shadowed onto /etc by the OS)
opt/cni/bin/      bridge, host-local, flannel, …  (cni-plugins only)
```

`EXTENSION_RELOAD_MANAGER=1` is set in `extension-release` only for sysexts that contain systemd units, causing `systemd daemon-reload` on merge.  The `cni-plugins` sysext omits it.

## Building

Requires Go ≥ 1.26.

```sh
go build -o sysext-pkg ./cmd/sysext-pkg
```

## CLI

```
sysext-pkg list [component]
    Print available upstream versions (newest first).

sysext-pkg build <component> <version> <arch>
    Download binaries, overlay static files, and write <component>-<version>-<arch>.raw.
    arch: amd64 | arm64

sysext-pkg push <component> <version> <arch>
    Push a pre-built .raw to the registry, tagged <version>-<arch>.

sysext-pkg release <component> <version>
    Full pipeline: build amd64 + arm64, push both, publish multi-arch OCI index.
    --skip-existing   skip if the version tag already exists
    --arches          comma-separated list (default: amd64,arm64)
```

Global flags:

```
--registry     OCI registry hostname  (default: ghcr.io; env: OCI_REGISTRY)
--repository   OCI repository path    (default: cellebyte/sysexts)
```

## OCI artifact layout

Each component is a separate OCI repository:

```
ghcr.io/<owner>/sysexts/kubernetes:v1.33.1          ← immutable index
ghcr.io/<owner>/sysexts/kubernetes:v1.33            ← mutable minor alias
```

Each index contains two manifests (`linux/amd64`, `linux/arm64`).  Each manifest holds a single layer — the raw squashfs `.raw` file.

## CI

`.github/workflows/release.yaml` provides:

- **Manual trigger** — release a specific component + version via `workflow_dispatch`
- **Daily schedule** (06:00 UTC) — checks the five most recent upstream versions for each component and releases any that are not yet in the registry

Requires `packages: write` permission on the GHCR token; no other secrets needed.

## Static files

Systemd units and config files live in `<component>.sysext/files/` and are merged over the staged binaries at build time.  The directory must contain only `usr/` or `opt/` subtrees; the build will reject `/etc`, `/var`, or `/run` with a hard error.

## Dependencies

| Library | Purpose |
|---|---|
| `github.com/diskfs/go-diskfs` | Pure-Go squashfs image creation |
| `oras.land/oras-go/v2` | OCI registry push/pull |
| `github.com/spf13/cobra` | CLI |

`conntrack` and `socat` are expected to be provided by the host OS and are not bundled.
