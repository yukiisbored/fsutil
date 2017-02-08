package archive

import (
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"testing"
	"time"

	_ "crypto/sha256"

	"github.com/docker/containerd/fs"
	"github.com/docker/containerd/fs/fstest"
	"github.com/pkg/errors"
)

const tarCmd = "/usr/bin/tar"

// baseApplier creates a basic filesystem layout
// with multiple types of files for basic tests.
var baseApplier = fstest.Apply(
	fstest.CreateDir("/etc/", 0755),
	fstest.CreateFile("/etc/hosts", []byte("127.0.0.1 localhost"), 0644),
	fstest.Link("/etc/hosts", "/etc/hosts.allow"),
	fstest.CreateDir("/usr/local/lib", 0755),
	fstest.CreateFile("/usr/local/lib/libnothing.so", []byte{0x00, 0x00}, 0755),
	fstest.Symlink("libnothing.so", "/usr/local/lib/libnothing.so.2"),
	fstest.CreateDir("/home", 0755),
	fstest.CreateDir("/home/derek", 0700),
)

func TestUnpack(t *testing.T) {
	requireTar(t)

	if err := testApply(baseApplier); err != nil {
		t.Fatalf("Test apply failed: %+v", err)
	}
}

func TestBaseDiff(t *testing.T) {
	requireTar(t)

	if err := testBaseDiff(baseApplier); err != nil {
		t.Fatalf("Test base diff failed: %+v", err)
	}
}

func TestDiffApply(t *testing.T) {
	as := []fstest.Applier{
		baseApplier,
		fstest.Apply(
			fstest.CreateFile("/etc/hosts", []byte("127.0.0.1 localhost.localdomain"), 0644),
			fstest.CreateFile("/etc/fstab", []byte("/dev/sda1\t/\text4\tdefaults 1 1\n"), 0600),
			fstest.CreateFile("/etc/badfile", []byte(""), 0666),
			fstest.CreateFile("/home/derek/.zshrc", []byte("#ZSH is just better\n"), 0640),
		),
		fstest.Apply(
			fstest.RemoveFile("/etc/badfile"),
			fstest.Rename("/home/derek", "/home/notderek"),
		),
		fstest.Apply(
			fstest.RemoveFile("/usr"),
			fstest.RemoveFile("/etc/hosts.allow"),
		),
		fstest.Apply(
			fstest.RemoveFile("/home"),
			fstest.CreateDir("/home/derek", 0700),
			fstest.CreateFile("/home/derek/.bashrc", []byte("#not going away\n"), 0640),
			// "/etc/hosts" must be touched to be hardlinked in same layer
			fstest.Chtime("/etc/hosts", time.Now()),
			fstest.Link("/etc/hosts", "/etc/hosts.allow"),
		),
	}

	if err := testDiffApply(as...); err != nil {
		t.Fatalf("Test diff apply failed: %+v", err)
	}
}

func testApply(a fstest.Applier) error {
	td, err := ioutil.TempDir("", "test-apply-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(td)
	dest, err := ioutil.TempDir("", "test-apply-dest-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(dest)

	if err := a.Apply(td); err != nil {
		return errors.Wrap(err, "failed to apply filesystem changes")
	}

	tarArgs := []string{"c", "-C", td}
	names, err := readDirNames(td)
	if err != nil {
		return errors.Wrap(err, "failed to read directory names")
	}
	tarArgs = append(tarArgs, names...)

	cmd := exec.Command(tarCmd, tarArgs...)

	arch, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "failed to create stdout pipe")
	}

	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, "failed to start command")
	}

	if _, err := ApplyDiffTar(context.Background(), dest, arch); err != nil {
		return errors.Wrap(err, "failed to apply tar stream")
	}

	return fstest.CheckDirectoryEqual(td, dest)
}

func testBaseDiff(a fstest.Applier) error {
	td, err := ioutil.TempDir("", "test-base-diff-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(td)
	dest, err := ioutil.TempDir("", "test-base-diff-dest-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(dest)

	if err := a.Apply(td); err != nil {
		return errors.Wrap(err, "failed to apply filesystem changes")
	}

	arch := DiffTarStream(context.Background(), "", td)

	cmd := exec.Command(tarCmd, "x", "-C", dest)
	cmd.Stdin = arch
	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "tar command failed")
	}

	return fstest.CheckDirectoryEqual(td, dest)
}

func testDiffApply(as ...fstest.Applier) error {
	base, err := ioutil.TempDir("", "test-diff-apply-base-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(base)
	dest, err := ioutil.TempDir("", "test-diff-apply-dest-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(dest)

	ctx := context.Background()
	for i, a := range as {
		if err := diffApply(ctx, a, base, dest); err != nil {
			return errors.Wrapf(err, "diff apply failed at layer %d", i)
		}
	}
	return nil
}

// diffApply applies the given changes on the base and
// computes the diff and applies to the dest.
func diffApply(ctx context.Context, a fstest.Applier, base, dest string) error {
	baseCopy, err := ioutil.TempDir("", "test-diff-apply-copy-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(baseCopy)
	if err := fs.CopyDir(baseCopy, base); err != nil {
		return errors.Wrap(err, "failed to copy base")
	}

	if err := a.Apply(base); err != nil {
		return errors.Wrap(err, "failed to apply changes to base")
	}

	if _, err := ApplyDiffTar(ctx, dest, DiffTarStream(ctx, baseCopy, base)); err != nil {
		return errors.Wrap(err, "failed to apply tar stream")
	}

	return fstest.CheckDirectoryEqual(base, dest)
}

func readDirNames(p string) ([]string, error) {
	fis, err := ioutil.ReadDir(p)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(fis))
	for i, fi := range fis {
		names[i] = fi.Name()
	}
	return names, nil
}

func requireTar(t *testing.T) {
	if _, err := os.Stat(tarCmd); err != nil {
		if os.IsNotExist(err) {
			t.Skipf("%s not found, skipping", tarCmd)
		} else {
			t.Fatalf("Unable to stat %s: %v", tarCmd, err)
		}
	}
}

func requireRoot(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatalf("Unable to get current user: %v", err)
	}
	if u.Uid != "0" {
		t.Skipf("test requires root, skipping")
	}
}
