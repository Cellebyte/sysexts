package internal_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/squashfs"

	"github.com/cellebyte/sysext-pkg/internal"
)

// hostArch returns the sysext arch token for the current host.
// internal.SysextArch maps "amd64"→"x86-64", "arm64"→"arm64".
func hostArch(t *testing.T) string {
	t.Helper()
	a := runtime.GOARCH
	if _, ok := internal.SysextArch[a]; !ok {
		t.Skipf("unsupported host arch for sysext tests: %s", a)
	}
	return a
}

// buildStagingDir creates a minimal staging directory with a test binary.
func buildStagingDir(t *testing.T) string {
	t.Helper()
	staging := t.TempDir()
	binDir := filepath.Join(staging, "usr", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(binDir, "hello-sysext"),
		[]byte("#!/bin/sh\necho hello-from-sysext\n"),
		0755,
	); err != nil {
		t.Fatal(err)
	}
	return staging
}

// ── unit tests ────────────────────────────────────────────────────────────────

// TestOutputFileName verifies the naming convention.
func TestOutputFileName(t *testing.T) {
	cases := []struct {
		name, version, arch, want string
	}{
		{"kubernetes", "v1.33.1", "amd64", "kubernetes-v1.33.1-amd64.raw"},
		{"k3s", "v1.32.4+k3s1", "arm64", "k3s-v1.32.4_k3s1-arm64.raw"},
		{"cni-plugins", "v1.6.2", "amd64", "cni-plugins-v1.6.2-amd64.raw"},
	}
	for _, tc := range cases {
		got := internal.OutputFileName(tc.name, tc.version, tc.arch)
		if got != tc.want {
			t.Errorf("OutputFileName(%q,%q,%q) = %q, want %q",
				tc.name, tc.version, tc.arch, got, tc.want)
		}
	}
}

// TestCreateSysext_Size verifies the output is a non-empty file padded to a
// 512-byte boundary (required by the kernel loop device).
func TestCreateSysext_Size(t *testing.T) {
	arch := hostArch(t)
	raw := filepath.Join(t.TempDir(), "test.raw")

	if err := internal.CreateSysext(buildStagingDir(t), raw, "test", arch); err != nil {
		t.Fatal("CreateSysext:", err)
	}

	fi, err := os.Stat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Fatal("output file is empty")
	}
	if fi.Size()%4096 != 0 {
		t.Errorf("output size %d is not a multiple of 4096 (kernel squashfs 1KB-read boundary requirement)", fi.Size())
	}
}

// TestCreateSysext_ExtensionRelease verifies the mandatory
// usr/lib/extension-release.d/extension-release.<name> file is present and
// has the correct ID=_any and ARCHITECTURE= fields.
//
// The caller (cmd/sysext-pkg) is expected to pass the filename-without-.raw
// as name so that systemd-sysext's name-from-filename matching works.
func TestCreateSysext_ExtensionRelease(t *testing.T) {
	arch := hostArch(t)
	// Use a version-qualified name as the CLI does: <component>-<version>-<arch>
	sysextName := "kubernetes-v1.36.1-" + arch
	raw := filepath.Join(t.TempDir(), sysextName+".raw")

	if err := internal.CreateSysext(buildStagingDir(t), raw, sysextName, arch); err != nil {
		t.Fatal("CreateSysext:", err)
	}

	// extension-release filename must match the image filename (minus .raw).
	content := readSquashfsFile(t, raw,
		"usr/lib/extension-release.d/extension-release."+sysextName)

	requireLine(t, content, "ID=_any",
		"extension-release must contain ID=_any for OS-agnostic sysext")

	wantArch := internal.SysextArch[arch]
	requireLine(t, content, "ARCHITECTURE="+wantArch,
		"extension-release ARCHITECTURE must match requested arch")
}

// TestCreateSysext_BinaryPresent verifies that a file placed in the staging
// directory ends up inside the squashfs at the correct path.
func TestCreateSysext_BinaryPresent(t *testing.T) {
	arch := hostArch(t)
	raw := filepath.Join(t.TempDir(), "test.raw")

	if err := internal.CreateSysext(buildStagingDir(t), raw, "test", arch); err != nil {
		t.Fatal("CreateSysext:", err)
	}

	content := readSquashfsFile(t, raw, "usr/bin/hello-sysext")
	if !strings.Contains(content, "echo hello-from-sysext") {
		t.Errorf("binary content unexpected: %q", content)
	}
}

// TestCreateSysext_BinaryMode verifies that the staged binary keeps its
// executable bit (0755) inside the squashfs.
func TestCreateSysext_BinaryMode(t *testing.T) {
	arch := hostArch(t)
	raw := filepath.Join(t.TempDir(), "test.raw")

	if err := internal.CreateSysext(buildStagingDir(t), raw, "test", arch); err != nil {
		t.Fatal("CreateSysext:", err)
	}

	fs := openSquashfs(t, raw)
	info, err := fs.Stat("usr/bin/hello-sysext")
	if err != nil {
		t.Fatal("stat usr/bin/hello-sysext:", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("hello-sysext mode = %v, want executable bits set", info.Mode())
	}
}

// TestCreateSysext_Symlink verifies that symlinks in the staging directory
// are preserved inside the squashfs (go-diskfs os.Symlink workaround).
func TestCreateSysext_Symlink(t *testing.T) {
	arch := hostArch(t)
	staging := buildStagingDir(t)

	// Add a symlink: usr/bin/hello-alias → hello-sysext
	if err := os.Symlink("hello-sysext",
		filepath.Join(staging, "usr", "bin", "hello-alias")); err != nil {
		t.Fatal("symlink:", err)
	}

	raw := filepath.Join(t.TempDir(), "test.raw")
	if err := internal.CreateSysext(staging, raw, "test", arch); err != nil {
		t.Fatal("CreateSysext:", err)
	}

	fs := openSquashfs(t, raw)
	info, err := fs.Stat("usr/bin/hello-alias")
	if err != nil {
		t.Fatal("stat hello-alias:", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("hello-alias mode = %v, expected symlink", info.Mode())
	}
}

// TestCreateSysext_UnknownArch verifies that an unsupported arch returns an
// error rather than silently producing a broken image.
func TestCreateSysext_UnknownArch(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "test.raw")
	err := internal.CreateSysext(buildStagingDir(t), raw, "test", "mips")
	if err == nil {
		t.Error("expected error for unsupported arch, got nil")
	}
}

// ── systemd integration tests ─────────────────────────────────────────────────

// requireSystemdSysext skips the test if systemd-sysext is not in PATH.
func requireSystemdSysext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemd-sysext"); err != nil {
		t.Skip("systemd-sysext not in PATH")
	}
}

// buildSysextRoot creates a minimal fake OS root with a sysext placed in
// var/lib/extensions and returns the root path.
func buildSysextRoot(t *testing.T, rawFile string) string {
	t.Helper()
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "usr", "lib"), 0755))
	must(t, os.MkdirAll(filepath.Join(root, "usr", "bin"), 0755))
	must(t, os.MkdirAll(filepath.Join(root, "var", "lib", "extensions"), 0755))
	// ID=_any means no OS version matching; any systemd host accepts it.
	must(t, os.WriteFile(
		filepath.Join(root, "usr", "lib", "os-release"),
		[]byte("ID=_any\nVERSION_ID=1\nNAME=TestOS\n"),
		0644,
	))
	data, err := os.ReadFile(rawFile)
	must(t, err)
	must(t, os.WriteFile(
		filepath.Join(root, "var", "lib", "extensions", "test-sysext.raw"),
		data, 0644,
	))
	return root
}

// TestSystemdSysextList verifies that systemd-sysext can parse the image
// metadata and report it in "list" output — no root required.
func TestSystemdSysextList(t *testing.T) {
	requireSystemdSysext(t)
	arch := hostArch(t)

	raw := filepath.Join(t.TempDir(), "test-sysext.raw")
	must(t, internal.CreateSysext(buildStagingDir(t), raw, "test-sysext", arch))

	root := buildSysextRoot(t, raw)

	out, err := exec.Command("systemd-sysext", "--root="+root, "list").CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-sysext list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "test-sysext") {
		t.Errorf("expected 'test-sysext' in list output, got:\n%s", out)
	}
}

// TestSystemdSysextMerge verifies the full cycle:
//  1. CreateSysext produces a valid image
//  2. systemd-sysext merge successfully overlays it into the fake root
//  3. The binary from the sysext is visible at <root>/usr/bin/hello-sysext
//  4. systemd-sysext unmerge cleans up
//
// Requires sudo because merge uses overlayfs / bind-mounts.
func TestSystemdSysextMerge(t *testing.T) {
	requireSystemdSysext(t)
	arch := hostArch(t)

	if os.Getenv("SYSEXT_TEST_SUDO") == "" {
		// Only run when the caller explicitly opts in, to avoid surprising
		// failures in environments without passwordless sudo.
		t.Skip("set SYSEXT_TEST_SUDO=1 to run systemd-sysext merge test")
	}

	raw := filepath.Join(t.TempDir(), "test-sysext.raw")
	must(t, internal.CreateSysext(buildStagingDir(t), raw, "test-sysext", arch))

	root := buildSysextRoot(t, raw)

	// Merge.
	mergeOut, mergeErr := exec.Command(
		"sudo", "systemd-sysext", "--root="+root, "merge",
	).CombinedOutput()
	t.Logf("merge output: %s", mergeOut)
	if mergeErr != nil {
		t.Fatalf("merge failed: %v", mergeErr)
	}

	// Ensure unmerge runs even if assertions fail.
	t.Cleanup(func() {
		exec.Command("sudo", "systemd-sysext", "--root="+root, "unmerge").Run() //nolint:errcheck
	})

	if !strings.Contains(string(mergeOut), "test-sysext") {
		t.Errorf("expected 'test-sysext' in merge output, got:\n%s", mergeOut)
	}

	// The binary from the sysext must be visible inside the merged root.
	if _, err := os.Stat(filepath.Join(root, "usr", "bin", "hello-sysext")); err != nil {
		t.Errorf("hello-sysext not visible after merge: %v", err)
	}
}

// ── validate / reload-manager tests ───────────────────────────────────────────

// buildStagingDirWithUnits extends buildStagingDir with a minimal systemd
// unit file so that hasSystemdUnits() returns true.
func buildStagingDirWithUnits(t *testing.T) string {
	t.Helper()
	staging := buildStagingDir(t)
	unitDir := filepath.Join(staging, "usr", "lib", "systemd", "system")
	must(t, os.MkdirAll(unitDir, 0755))
	must(t, os.WriteFile(
		filepath.Join(unitDir, "hello-sysext.service"),
		[]byte("[Unit]\nDescription=Hello\n[Install]\nWantedBy=multi-user.target\n"),
		0644,
	))
	return staging
}

// TestCreateSysext_ValidateStagingDir verifies that CreateSysext returns an
// error when the staging directory contains /etc — which systemd-sysext
// silently ignores.
func TestCreateSysext_ValidateStagingDir(t *testing.T) {
	arch := hostArch(t)
	staging := buildStagingDir(t)

	// Drop a config file into /etc — the common mistake we want to catch.
	must(t, os.MkdirAll(filepath.Join(staging, "etc", "containerd"), 0755))
	must(t, os.WriteFile(
		filepath.Join(staging, "etc", "containerd", "config.toml"),
		[]byte("version = 3\n"),
		0644,
	))

	raw := filepath.Join(t.TempDir(), "test.raw")
	err := internal.CreateSysext(staging, raw, "test", arch)
	if err == nil {
		t.Fatal("expected error for staging dir containing /etc, got nil")
	}
	if !strings.Contains(err.Error(), "/etc") {
		t.Errorf("error should mention /etc; got: %v", err)
	}
}

// TestCreateSysext_ReloadManager_WithUnits verifies that EXTENSION_RELOAD_MANAGER=1
// is written into the extension-release file when the staging tree contains
// at least one systemd unit.
func TestCreateSysext_ReloadManager_WithUnits(t *testing.T) {
	arch := hostArch(t)
	sysextName := "withunits-" + arch
	raw := filepath.Join(t.TempDir(), sysextName+".raw")

	must(t, internal.CreateSysext(buildStagingDirWithUnits(t), raw, sysextName, arch))

	content := readSquashfsFile(t, raw,
		"usr/lib/extension-release.d/extension-release."+sysextName)
	requireLine(t, content, "EXTENSION_RELOAD_MANAGER=1",
		"extension-release must contain EXTENSION_RELOAD_MANAGER=1 when units are present")
}

// TestCreateSysext_ReloadManager_WithoutUnits verifies that
// EXTENSION_RELOAD_MANAGER is omitted when the staging tree has no systemd
// unit files (e.g. a binary-only sysext like cni-plugins).
func TestCreateSysext_ReloadManager_WithoutUnits(t *testing.T) {
	arch := hostArch(t)
	sysextName := "nounits-" + arch
	raw := filepath.Join(t.TempDir(), sysextName+".raw")

	// buildStagingDir has only usr/bin/hello-sysext — no units.
	must(t, internal.CreateSysext(buildStagingDir(t), raw, sysextName, arch))

	content := readSquashfsFile(t, raw,
		"usr/lib/extension-release.d/extension-release."+sysextName)
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "EXTENSION_RELOAD_MANAGER=1" {
			t.Errorf("EXTENSION_RELOAD_MANAGER=1 must not appear for a unit-less sysext; extension-release:\n%s", content)
			return
		}
	}
}

// ── squashfs read helpers ──────────────────────────────────────────────────────

// openSquashfs opens a .raw squashfs image for reading via go-diskfs.
func openSquashfs(t *testing.T, rawFile string) *squashfs.FileSystem {
	t.Helper()
	f, err := os.Open(rawFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	b := backendfile.New(f, true)
	fs, err := squashfs.Read(b, 0, 0, 131072)
	if err != nil {
		t.Fatal("squashfs.Read:", err)
	}
	return fs
}

// readSquashfsFile reads a file out of a .raw squashfs image and returns
// its contents as a string.
func readSquashfsFile(t *testing.T, rawFile, path string) string {
	t.Helper()
	fs := openSquashfs(t, rawFile)
	rfile, err := fs.OpenFile(path, os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", path, err)
	}
	data, err := io.ReadAll(rfile)
	if err != nil {
		t.Fatalf("ReadAll(%q): %v", path, err)
	}
	return string(data)
}

// walkSquashfs calls fn for every file in the squashfs image (recursive).
func walkSquashfs(t *testing.T, rawFile string, fn func(*testing.T, os.FileInfo)) {
	t.Helper()
	fs := openSquashfs(t, rawFile)
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := fs.ReadDir(dir)
		if err != nil {
			t.Errorf("ReadDir(%q): %v", dir, err)
			return
		}
		for _, e := range entries {
			p := dir + "/" + e.Name()
			if dir == "." {
				p = e.Name()
			}
			info, err := e.Info()
			if err != nil {
				t.Errorf("Info(%q): %v", p, err)
				continue
			}
			fn(t, info)
			if e.IsDir() {
				walk(p)
			}
		}
	}
	walk(".")
}

// requireLine checks that content contains a line equal to want.
func requireLine(t *testing.T, content, want, msg string) {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Errorf("%s\nwant line %q in:\n%s", msg, want, content)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
