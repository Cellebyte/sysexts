package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---- Kubernetes upstream ------------------------------------------------

// k8sSchedule mirrors the structure of
// https://raw.githubusercontent.com/kubernetes/website/main/data/releases/schedule.yaml
type k8sSchedule struct {
	Schedules []struct {
		Release         string `yaml:"release"`
		PreviousPatches []struct {
			Release string `yaml:"release"`
		} `yaml:"previousPatches"`
	} `yaml:"schedules"`
}

const k8sScheduleURL = "https://raw.githubusercontent.com/kubernetes/website/main/data/releases/schedule.yaml"

// ListKubernetesVersions returns all known patch releases, newest first.
func ListKubernetesVersions(ctx context.Context) ([]string, error) {
	return fetchK8sVersions(ctx, false)
}

// LatestKubernetesVersions returns the latest patch of every active minor.
func LatestKubernetesVersions(ctx context.Context) ([]string, error) {
	return fetchK8sVersions(ctx, true)
}

func fetchK8sVersions(ctx context.Context, latestOnly bool) ([]string, error) {
	data, err := httpGet(ctx, k8sScheduleURL)
	if err != nil {
		return nil, fmt.Errorf("fetch k8s schedule: %w", err)
	}

	var sched k8sSchedule
	if err := yaml.Unmarshal(data, &sched); err != nil {
		return nil, fmt.Errorf("parse k8s schedule: %w", err)
	}

	var versions []string
	for _, entry := range sched.Schedules {
		if latestOnly {
			// Latest patch of this minor: first previousPatches entry, or
			// the release itself (with ".0" appended) if no patches exist yet.
			if len(entry.PreviousPatches) > 0 {
				versions = append(versions, "v"+entry.PreviousPatches[0].Release)
			} else {
				versions = append(versions, "v"+entry.Release+".0")
			}
		} else {
			for _, p := range entry.PreviousPatches {
				versions = append(versions, "v"+p.Release)
			}
		}
	}

	sort.Sort(sort.Reverse(semverSlice(versions)))
	return versions, nil
}

// ---- EKS Distro ---------------------------------------------------------

const eksdReleasesURL = "https://api.github.com/repos/aws/eks-distro/releases"

// ListEKSDVersions returns available EKS-D releases, newest first.
// The tag format is "kubernetes-1-NN-eks-M"; we expose them as-is.
func ListEKSDVersions(ctx context.Context) ([]string, error) {
	return listGitHubReleases(ctx, eksdReleasesURL)
}

// ---- containerd ---------------------------------------------------------

const containerdReleasesURL = "https://api.github.com/repos/containerd/containerd/releases"

// ListContainerdVersions returns available containerd releases, newest first.
// Strips the leading "v" to match the download URL pattern.
func ListContainerdVersions(ctx context.Context) ([]string, error) {
	tags, err := listGitHubReleases(ctx, containerdReleasesURL)
	if err != nil {
		return nil, err
	}
	// Filter out the containerd/containerd API sub-module releases (prefixed "api/")
	var filtered []string
	for _, t := range tags {
		if !strings.HasPrefix(t, "api/") {
			filtered = append(filtered, strings.TrimPrefix(t, "v"))
		}
	}
	return filtered, nil
}

// ---- CRI-O --------------------------------------------------------------

const crioReleasesURL = "https://api.github.com/repos/cri-o/cri-o/releases"

// ListCRIOVersions returns available CRI-O releases, newest first.
func ListCRIOVersions(ctx context.Context) ([]string, error) {
	return listGitHubReleases(ctx, crioReleasesURL)
}

// ---- k3s ----------------------------------------------------------------

const k3sReleasesURL = "https://api.github.com/repos/k3s-io/k3s/releases"

// ListK3sVersions returns available k3s releases, newest first.
func ListK3sVersions(ctx context.Context) ([]string, error) {
	return listGitHubReleases(ctx, k3sReleasesURL)
}

// ---- CNI plugins --------------------------------------------------------

const cniReleasesURL = "https://api.github.com/repos/containernetworking/plugins/releases"

// ListCNIPluginsVersions returns available CNI plugin releases, newest first.
func ListCNIPluginsVersions(ctx context.Context) ([]string, error) {
	return listGitHubReleases(ctx, cniReleasesURL)
}

// ---- helpers ------------------------------------------------------------

// githubRelease is the minimal subset of the GitHub releases API response.
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

// listGitHubReleases fetches release tags from a GitHub releases API URL,
// skipping pre-releases and drafts, returning tags newest first.
func listGitHubReleases(ctx context.Context, apiURL string) ([]string, error) {
	var allTags []string

	// GitHub paginates at 30 per page; fetch up to 5 pages (150 releases).
	for page := 1; page <= 5; page++ {
		url := fmt.Sprintf("%s?per_page=30&page=%d", apiURL, page)
		data, err := httpGet(ctx, url)
		if err != nil {
			return nil, err
		}

		var releases []githubRelease
		if err := json.Unmarshal(data, &releases); err != nil {
			return nil, fmt.Errorf("parse releases from %s: %w", apiURL, err)
		}
		if len(releases) == 0 {
			break
		}

		for _, r := range releases {
			if !r.Prerelease && !r.Draft {
				allTags = append(allTags, r.TagName)
			}
		}
	}

	return allTags, nil
}

// httpGet performs a simple GET with optional GITHUB_TOKEN auth and returns
// the response body.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// semverSlice implements sort.Interface for version strings like "v1.33.1".
type semverSlice []string

func (s semverSlice) Len() int      { return len(s) }
func (s semverSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s semverSlice) Less(i, j int) bool {
	return semverLess(s[i], s[j])
}

// semverLess returns true if a < b using simple major.minor.patch comparison.
func semverLess(a, b string) bool {
	return semverTuple(a) < semverTuple(b)
}

// semverTuple converts "v1.33.1" → a comparable integer tuple string.
// We use a zero-padded string so lexicographic order equals version order.
func semverTuple(v string) string {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	return fmt.Sprintf("%05s.%05s.%05s", parts[0], parts[1], parts[2])
}
