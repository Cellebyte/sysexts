// Package internal contains all build pipeline logic for k8s-sysext.
package internal

import (
	"context"
	"fmt"
	"strings"
)

// Role describes what a sysext component provides to the host.
type Role string

const (
	RoleKubernetesNode   Role = "kubernetes-node"   // kubelet, kubeadm, kubectl
	RoleContainerRuntime Role = "container-runtime" // CRI daemon + runc
	RoleCNI              Role = "cni"               // CNI plugins at /opt/cni/bin/
)

// Component describes a sysext image: what it provides, what it conflicts
// with, and how to build it.
type Component struct {
	// Name is the image name, e.g. "kubernetes", "containerd", "k3s".
	Name string

	// Roles lists every capability this component provides.
	Roles []Role

	// Exclusive means this component cannot be combined with any other
	// component (k3s bundles kubernetes-node + container-runtime + cni).
	Exclusive bool

	// Conflicts lists component names that must not be active at the same
	// time (e.g. "containerd" conflicts with "crio").
	Conflicts []string

	// CRISocket is the Unix socket path this runtime exposes.
	// Empty for components that are not a container runtime.
	CRISocket string

	// ListVersions returns available upstream versions, newest first.
	ListVersions func(ctx context.Context) ([]string, error)

	// Fetch downloads all binaries and places them under stagingDir,
	// replicating the final filesystem layout (usr/bin/, opt/cni/bin/, …).
	Fetch func(ctx context.Context, version, arch, stagingDir string) error

	// StaticFilesDir is the path (relative to the repo root) that holds
	// pre-written systemd units and config files to merge into stagingDir.
	// Empty for components that have no static files (cni-plugins).
	StaticFilesDir string
}

// Registry is the single source of truth for every supported component.
// Adding a new runtime means adding one entry here plus a Fetch function.
var Registry = map[string]*Component{
	"kubernetes": {
		Name:           "kubernetes",
		Roles:          []Role{RoleKubernetesNode},
		Conflicts:      []string{"eksd", "k3s"},
		ListVersions:   ListKubernetesVersions,
		Fetch:          FetchKubernetes,
		StaticFilesDir: "kubernetes.sysext/files",
	},
	"eksd": {
		Name:           "eksd",
		Roles:          []Role{RoleKubernetesNode},
		Conflicts:      []string{"kubernetes", "k3s"},
		ListVersions:   ListEKSDVersions,
		Fetch:          FetchEKSD,
		StaticFilesDir: "eksd.sysext/files",
	},
	"containerd": {
		Name:           "containerd",
		Roles:          []Role{RoleContainerRuntime},
		Conflicts:      []string{"crio", "k3s"},
		CRISocket:      "/run/containerd/containerd.sock",
		ListVersions:   ListContainerdVersions,
		Fetch:          FetchContainerd,
		StaticFilesDir: "containerd.sysext/files",
	},
	"crio": {
		Name:           "crio",
		Roles:          []Role{RoleContainerRuntime},
		Conflicts:      []string{"containerd", "k3s"},
		CRISocket:      "/var/run/crio/crio.sock",
		ListVersions:   ListCRIOVersions,
		Fetch:          FetchCRIO,
		StaticFilesDir: "crio.sysext/files",
	},
	"k3s": {
		Name:           "k3s",
		Roles:          []Role{RoleKubernetesNode, RoleContainerRuntime, RoleCNI},
		Exclusive:      true,
		CRISocket:      "/run/k3s/containerd/containerd.sock",
		ListVersions:   ListK3sVersions,
		Fetch:          FetchK3s,
		StaticFilesDir: "k3s.sysext/files",
	},
	"cni-plugins": {
		Name:           "cni-plugins",
		Roles:          []Role{RoleCNI},
		ListVersions:   ListCNIPluginsVersions,
		Fetch:          FetchCNIPlugins,
		StaticFilesDir: "", // no systemd units — purely passive binaries
	},
}

// ValidateCombination returns an error if the given component names cannot
// be activated together on the same node.
func ValidateCombination(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("no components specified")
	}

	for _, name := range names {
		c, ok := Registry[name]
		if !ok {
			return fmt.Errorf("unknown component %q (known: %s)", name, knownNames())
		}

		// An exclusive component (k3s) cannot be combined with anything else.
		if c.Exclusive && len(names) > 1 {
			return fmt.Errorf(
				"%q is self-contained and cannot be combined with: %s",
				name, strings.Join(without(names, name), ", "),
			)
		}

		// Explicit conflict list.
		for _, conflict := range c.Conflicts {
			for _, other := range names {
				if other == conflict {
					return fmt.Errorf(
						"%q and %q cannot be activated together", name, conflict,
					)
				}
			}
		}
	}

	// At most one component may fill each role.
	roleSeen := map[Role]string{}
	for _, name := range names {
		c := Registry[name]
		for _, role := range c.Roles {
			if prev, exists := roleSeen[role]; exists {
				return fmt.Errorf(
					"role conflict: both %q and %q provide %q", prev, name, role,
				)
			}
			roleSeen[role] = name
		}
	}

	return nil
}

// Get returns a component by name or an error if it does not exist.
func Get(name string) (*Component, error) {
	c, ok := Registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown component %q (known: %s)", name, knownNames())
	}
	return c, nil
}

// knownNames returns a sorted, comma-separated list of registered names.
func knownNames() string {
	names := make([]string, 0, len(Registry))
	for name := range Registry {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// without returns a copy of slice without the given element.
func without(slice []string, elem string) []string {
	out := make([]string, 0, len(slice))
	for _, s := range slice {
		if s != elem {
			out = append(out, s)
		}
	}
	return out
}
