package libzfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCommands struct {
	t   *testing.T
	dir string
}

// fakeZFS installs zfs and zpool commands in PATH which print the content of <command>.out for any
// arguments and log their arguments, one call per line, to calls.log.
func fakeZFS(t *testing.T, zfsOut, zpoolOut string) fakeCommands {
	t.Helper()
	dir := t.TempDir()
	for cmd, out := range map[string]string{"zfs": zfsOut, "zpool": zpoolOut} {
		if err := os.WriteFile(filepath.Join(dir, cmd+".out"), []byte(out), 0600); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho \"" + cmd + " $*\" >> " + filepath.Join(dir, "calls.log") + "\ncat " + filepath.Join(dir, cmd+".out") + "\n"
		if err := os.WriteFile(filepath.Join(dir, cmd), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return fakeCommands{t: t, dir: dir}
}

// calls returns the commands called so far.
func (f fakeCommands) calls() []string {
	b, err := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	if err != nil {
		return nil
	}
	return splitLines(string(b))
}

// setZFSOutput changes what the zfs command prints.
func (f fakeCommands) setZFSOutput(out string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "zfs.out"), []byte(out), 0600); err != nil {
		f.t.Fatal(err)
	}
}

const scanOutput = `rpool	type	filesystem	-
rpool	createtxg	1	-
rpool	canmount	off	local
rpool	mountpoint	/	local
rpool	origin	-	-
rpool	mounted	no	-
rpool	creation	1600000000	-
rpool	volsize	-	-
rpool	com.ubuntu.zsys:bootfs	-	-
rpool/ROOT	type	filesystem	-
rpool/ROOT	createtxg	5	-
rpool/ROOT	canmount	on	default
rpool/ROOT	mountpoint	/ROOT	inherited from rpool
rpool/ROOT	origin	-	-
rpool/ROOT	com.ubuntu.zsys:bootfs	yes	local
rpool/ROOT@new	type	snapshot	-
rpool/ROOT@new	createtxg	30	-
rpool/ROOT@new	canmount	-	-
rpool/ROOT@new	com.ubuntu.zsys:bootfs	yes	inherited from rpool/ROOT
rpool/ROOT@old	type	snapshot	-
rpool/ROOT@old	createtxg	20	-
rpool/ROOT@old	com.ubuntu.zsys:bootfs	value with	tab	local
rpool/ROOT/clone	type	filesystem	-
rpool/ROOT/clone	createtxg	40	-
rpool/ROOT/clone	origin	rpool/ROOT@old	-
rpool/vol	type	volume	-
rpool/vol	createtxg	10	-
rpool/vol	volsize	819200	local
`

func TestDatasetOpenAll(t *testing.T) {
	f := fakeZFS(t, scanOutput, "")

	ds, err := Adapter{}.DatasetOpenAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("expected one root dataset, got %d", len(ds))
	}
	if c := f.calls(); len(c) != 1 || !strings.Contains(c[0], "com.ubuntu.zsys:bootfs") || strings.Contains(c[0], " all") {
		t.Errorf("expected one zfs get of the scanned properties, got %q", c)
	}

	root := ds[0]
	if got := (*root.Properties())[DatasetPropMountpoint]; got != (Property{Value: "/", Source: "local"}) {
		t.Errorf("rpool mountpoint: got %v", got)
	}
	if _, ok := (*root.Properties())[DatasetPropOrigin]; ok {
		t.Error("origin should be absent on a dataset which isn't a clone")
	}
	if _, ok := (*root.Properties())[DatasetPropVolsize]; ok {
		t.Error("volsize should be absent on a filesystem")
	}
	if got, _ := root.GetUserProperty(BootfsProp); got != (Property{Value: "-", Source: "none"}) {
		t.Errorf("unset user property: got %v", got)
	}

	children := root.Children()
	var names []string
	for _, c := range children {
		names = append(names, (*c.Properties())[DatasetPropName].Value)
	}
	if strings.Join(names, ",") != "rpool/ROOT,rpool/vol" {
		t.Fatalf("rpool children: got %v", names)
	}
	if children[1].Type() != DatasetTypeVolume {
		t.Errorf("rpool/vol: expected a volume, got %d", children[1].Type())
	}

	rootfs := children[0]
	if got := (*rootfs.Properties())[DatasetPropMountpoint]; got != (Property{Value: "/ROOT", Source: "inherited"}) {
		t.Errorf("inherited native property: got %v", got)
	}
	names = nil
	for _, c := range rootfs.Children() {
		names = append(names, (*c.Properties())[DatasetPropName].Value)
	}
	// Filesystems first, then snapshots by creation order
	if strings.Join(names, ",") != "rpool/ROOT/clone,rpool/ROOT@old,rpool/ROOT@new" {
		t.Fatalf("rpool/ROOT children: got %v", names)
	}

	clone, old, new := rootfs.Children()[0], rootfs.Children()[1], rootfs.Children()[2]
	if got := (*clone.Properties())[DatasetPropOrigin]; got != (Property{Value: "rpool/ROOT@old", Source: "none"}) {
		t.Errorf("clone origin: got %v", got)
	}
	if !new.IsSnapshot() || clone.IsSnapshot() {
		t.Error("IsSnapshot doesn't match dataset types")
	}
	if _, ok := (*new.Properties())[DatasetPropCanmount]; ok {
		t.Error("canmount should be absent on a snapshot")
	}
	if got, _ := new.GetUserProperty(BootfsProp); got != (Property{Value: "yes", Source: "rpool/ROOT"}) {
		t.Errorf("inherited user property should have its parent as source: got %v", got)
	}
	if got, _ := old.GetUserProperty(BootfsProp); got != (Property{Value: "value with\ttab", Source: "local"}) {
		t.Errorf("user property with a tab: got %v", got)
	}
}

func TestGetUserPropertyNotScanned(t *testing.T) {
	f := fakeZFS(t, "rpool\ttype\tfilesystem\t-\n", "")
	ds, err := Adapter{}.DatasetOpen("rpool")
	if err != nil {
		t.Fatal(err)
	}

	f.setZFSOutput("org:prop\tsome value\tinherited from parent\n")
	for i := 0; i < 2; i++ {
		got, err := ds.GetUserProperty("org:prop")
		if err != nil {
			t.Fatal(err)
		}
		if got != (Property{Value: "some value", Source: "parent"}) {
			t.Errorf("got %v", got)
		}
	}
	if c := f.calls(); len(c) != 2 {
		t.Errorf("expected the property to be read once then cached, got calls %q", c)
	}
}

func TestDatasetCreateDoesNotMount(t *testing.T) {
	f := fakeZFS(t, "rpool/new\ttype\tfilesystem\t-\n", "")
	a := Adapter{}
	_, err := a.DatasetCreate("rpool/new", DatasetTypeFilesystem, map[Prop]Property{
		DatasetPropMountpoint: {Value: "/new"},
		DatasetPropCanmount:   {Value: "noauto"},
		DatasetPropCreation:   {Value: "123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := f.calls(); len(c) < 1 || c[0] != "zfs create -u -o canmount=noauto -o mountpoint=/new rpool/new" {
		t.Errorf("unexpected create command: %q", c)
	}
}

func TestDestroyRefusesMountedFilesystem(t *testing.T) {
	f := fakeZFS(t, "yes\n", "")
	d := dZFSAdapter{&Dataset{Type: DatasetTypeFilesystem, Properties: map[Prop]Property{DatasetPropName: {Value: "rpool/ROOT"}}}}
	if err := d.Destroy(false); err == nil {
		t.Error("expected an error destroying a mounted filesystem")
	}
	for _, c := range f.calls() {
		if strings.HasPrefix(c, "zfs destroy") {
			t.Errorf("destroy shouldn't have been called: %q", c)
		}
	}

	withChildren := dZFSAdapter{&Dataset{Type: DatasetTypeFilesystem, Properties: map[Prop]Property{DatasetPropName: {Value: "rpool"}}, Children: []Dataset{{}}}}
	if err := withChildren.Destroy(false); err == nil {
		t.Error("expected an error destroying a dataset with children")
	}
}

func TestClonesMostRecentSnapshotFirst(t *testing.T) {
	fakeZFS(t, `rpool/ROOT@a	createtxg	10
rpool/ROOT@a	clones	rpool/c1,rpool/c2
rpool/ROOT@b	createtxg	30
rpool/ROOT@b	clones	rpool/c3
rpool/ROOT@c	createtxg	20
rpool/ROOT@c	clones	-
`, "")
	d := dZFSAdapter{&Dataset{Type: DatasetTypeFilesystem, Properties: map[Prop]Property{DatasetPropName: {Value: "rpool/ROOT"}}}}
	clones, err := d.Clones()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(clones, ",") != "rpool/c3,rpool/c1,rpool/c2" {
		t.Errorf("got %v", clones)
	}
}

func TestPoolOpen(t *testing.T) {
	fakeZFS(t, "", "altroot\t-\tdefault\ncapacity\t91\t-\nname\tbpool\t-\n")
	p, err := Adapter{}.PoolOpen("bpool")
	if err != nil {
		t.Fatal(err)
	}
	if p.Properties[PoolPropCapacity].Value != "91" || p.Properties[PoolPropAltroot].Value != "-" || p.Properties[PoolPropName].Value != "bpool" {
		t.Errorf("got %v", p.Properties)
	}
}
