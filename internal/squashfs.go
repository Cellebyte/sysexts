package internal

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/squashfs"
)

const squashfsBlockSize = int64(131072) // 128 KiB — common sysext default

// SysextArch maps our arch names to the ARCHITECTURE= value written into the
// extension-release metadata file.
var SysextArch = map[string]string{
	"amd64": "x86-64",
	"arm64": "arm64",
}

// CreateSysext builds a squashfs sysext image from stagingDir and writes it
// to outputFile.  All inodes are owned by root:root (uid=gid=0).
//
// stagingDir must already contain the final directory tree, e.g.:
//
//	staging/usr/bin/kubelet
//	staging/opt/cni/bin/bridge
//	staging/usr/lib/systemd/system/kubelet.service
//
// CreateSysext adds the mandatory extension-release metadata file before
// packing, so the caller does not need to create it.
func CreateSysext(stagingDir, outputFile, name, arch string) error {
	sysextArch, ok := SysextArch[arch]
	if !ok {
		return fmt.Errorf("unsupported arch %q (want amd64 or arm64)", arch)
	}

	// 0. Validate staging directory layout before packing.
	//    Catches directories that systemd-sysext silently ignores (/etc, /var,
	//    /run) so that misplaced config files fail loudly at build time.
	if err := validateStagingDir(stagingDir); err != nil {
		return err
	}

	// 1. Write extension-release metadata into the staging tree.
	if err := writeExtensionRelease(stagingDir, name, sysextArch); err != nil {
		return fmt.Errorf("write extension-release: %w", err)
	}

	// 2. Create the output file and build the squashfs backend from it.
	//    Use the same pattern as the go-diskfs test suite:
	//      os.Create → backendfile.New → squashfs.Create
	f, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	// Keep f open; squashfs.Create writes through the backend.
	defer f.Close()

	b := backendfile.New(f, false)
	sqfs, err := squashfs.Create(b, 0, 0, squashfsBlockSize)
	if err != nil {
		return fmt.Errorf("create squashfs: %w", err)
	}

	// 3. Populate the squashfs workspace from stagingDir.
	//    go-diskfs uses a temp directory as a staging workspace before
	//    finalising; we copy files there using os.* calls directly.
	ws := sqfs.Workspace()
	if err := populateWorkspace(ws, stagingDir); err != nil {
		return fmt.Errorf("populate workspace: %w", err)
	}

	// 4. Finalise — force all inodes to root:root ownership.
	uid, gid := uint32(0), uint32(0)
	fOpts := squashfs.FinalizeOptions{
		Compression: &squashfs.CompressorGzip{},
		FileUID:     &uid,
		FileGID:     &gid,
	}

	// Workaround for go-diskfs bug: Finalize resolves symlinks via
	// os.Readlink(e.path) where e.path is relative to the workspace
	// (e.g. "usr/bin/k3s"), but does not prepend the workspace root.
	// Changing CWD to ws makes the relative readlink resolve correctly.
	origDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	if err := os.Chdir(ws); err != nil {
		return fmt.Errorf("chdir to workspace: %w", err)
	}
	finalizeErr := sqfs.Finalize(fOpts)
	os.Chdir(origDir) // always restore, even on error
	if finalizeErr != nil {
		return fmt.Errorf("finalize squashfs: %w", finalizeErr)
	}

	// Pad the image to a 4096-byte boundary.
	//
	// systemd-sysext loop-mounts .raw images; the kernel squashfs driver reads
	// metadata (id table, inode table) using 1 KB-aligned I/O requests.  When
	// the id table lands in the very last sector of the loop device the driver
	// issues a 2-sector read that overruns the device boundary, causing
	// mount(2) to fail with "unable to read id index table".
	//
	// Padding to 4096 bytes (8 × 512-byte sectors) ensures the squashfs
	// metadata always has enough empty sectors after it regardless of image
	// size.
	if err := padTo4096(outputFile); err != nil {
		return fmt.Errorf("pad image: %w", err)
	}

	return nil
}

// padTo4096 appends zero bytes to path until its size is a multiple of 4096.
func padTo4096(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if pad := (4096 - fi.Size()%4096) % 4096; pad > 0 {
		_, err = f.Write(make([]byte, pad))
		return err
	}
	return nil
}

// writeExtensionRelease creates the mandatory
// usr/lib/extension-release.d/extension-release.<name> file inside dir.
//
// Using ID=_any makes the sysext OS-agnostic (works on Flatcar, CoreOS, any
// systemd host) and avoids version-pinning to a specific OS release.
//
// EXTENSION_RELOAD_MANAGER=1 is only written when the staging tree contains
// at least one systemd unit file.  That causes systemd to run daemon-reload
// when the sysext is merged, so newly provided units are immediately visible.
// For binary-only sysexts (e.g. cni-plugins) the field is omitted to avoid
// a needless daemon-reload on every merge.
func writeExtensionRelease(stagingDir, name, sysextArch string) error {
	releaseDir := filepath.Join(stagingDir, "usr", "lib", "extension-release.d")
	if err := os.MkdirAll(releaseDir, 0755); err != nil {
		return err
	}

	content := fmt.Sprintf("ID=_any\nARCHITECTURE=%s\n", sysextArch)
	if hasSystemdUnits(stagingDir) {
		content += "EXTENSION_RELOAD_MANAGER=1\n"
	}
	dest := filepath.Join(releaseDir, "extension-release."+name)
	return os.WriteFile(dest, []byte(content), 0644)
}

// hasSystemdUnits reports whether stagingDir/usr/lib/systemd/system/ contains
// any non-directory entries (unit files, drop-in .conf files, etc.).
func hasSystemdUnits(stagingDir string) bool {
	unitDir := filepath.Join(stagingDir, "usr", "lib", "systemd", "system")
	found := false
	_ = filepath.WalkDir(unitDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // dir may not exist — that's fine
		}
		if !d.IsDir() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// validateStagingDir checks that the staging directory only contains
// filesystem hierarchies that systemd-sysext actually merges (/usr, /opt).
// Directories like /etc, /var, and /run are silently ignored by
// systemd-sysext, so files placed there would have no effect; this function
// turns that silent failure into a hard build-time error.
func validateStagingDir(stagingDir string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return err
	}
	var problems []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch e.Name() {
		case "usr", "opt":
			// valid sysext overlay hierarchies
		case "etc":
			problems = append(problems,
				"/etc: sysexts cannot overlay /etc — move config files to usr/etc/")
		case "var":
			problems = append(problems,
				"/var: sysexts cannot overlay /var — use a tmpfiles.d config instead")
		case "run":
			problems = append(problems,
				"/run: sysexts cannot overlay /run")
		default:
			problems = append(problems, fmt.Sprintf(
				"/%s: unexpected top-level directory (sysexts only overlay /usr and /opt)",
				e.Name()))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid staging layout:\n  - %s",
			strings.Join(problems, "\n  - "))
	}
	return nil
}

// populateWorkspace copies the entire directory tree at srcDir into the
// squashfs workspace directory ws, preserving file modes and symlinks.
//
// go-diskfs workarounds applied here:
//   - Symlinks: os.Symlink() directly in workspace (squashfs.Symlink is ErrNotImplemented)
//   - Permissions: os.Chmod() after every regular file copy (OpenFile hardcodes 0644)
func populateWorkspace(ws, srcDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		dest := filepath.Join(ws, rel)

		// We need the raw FileInfo to detect symlinks; DirEntry.Type() only
		// reports os.ModeSymlink when the entry itself was lstat'd.
		// Use os.Lstat to get the accurate mode.
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		// Symlink — use os.Symlink directly (workaround #1).
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(dest) // remove stale entry if any
			return os.Symlink(target, dest)
		}

		if d.IsDir() {
			return os.MkdirAll(dest, info.Mode().Perm())
		}

		// Regular file — copy content then fix permissions (workaround #2).
		if err := copyFileContent(path, dest); err != nil {
			return err
		}
		return os.Chmod(dest, info.Mode().Perm())
	})
}

// copyFileContent copies the bytes of src to dest, creating dest if needed.
func copyFileContent(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// OutputFileName returns the conventional sysext image filename:
//
//	<name>-<version>-<arch>.raw
//
// e.g. "kubernetes-v1.33.1-amd64.raw"
func OutputFileName(name, version, arch string) string {
	// Sanitise version for use in a filename (k3s versions contain "+").
	safeVersion := strings.ReplaceAll(version, "+", "_")
	return fmt.Sprintf("%s-%s-%s.raw", name, safeVersion, arch)
}
