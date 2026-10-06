package libzfs

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Adapter talks to ZFS through the zfs and zpool commands.
// Their parsable (-Hp) output is stable across OpenZFS releases and packagings, unlike the libzfs ABI,
// so the same binary works with Ubuntu's zfsutils-linux as well as upstream OpenZFS packages.
type Adapter struct{}

var (
	datasetPropNames = map[Prop]string{
		DatasetPropName:       "name",
		DatasetPropCanmount:   "canmount",
		DatasetPropMountpoint: "mountpoint",
		DatasetPropOrigin:     "origin",
		DatasetPropMounted:    "mounted",
		DatasetPropCreation:   "creation",
		DatasetPropVolsize:    "volsize",
	}
	datasetPropsByName = func() map[string]Prop {
		m := make(map[string]Prop)
		for p, n := range datasetPropNames {
			m[n] = p
		}
		return m
	}()

	// scannedProps are read for all datasets in one go. Reading "all" properties is an order of magnitude slower.
	// Other user properties are read on demand.
	scannedProps = func() string {
		props := []string{"type", "createtxg"}
		for p, n := range datasetPropNames {
			if p != DatasetPropName {
				props = append(props, n)
			}
		}
		props = append(props, BootfsProp, LastUsedProp, BootfsDatasetsProp, LastBootedKernelProp,
			SnapshotCanmountProp, SnapshotMountpointProp)
		sort.Strings(props)
		return strings.Join(props, ",")
	}()

	poolPropNames = map[Prop]string{
		PoolPropName:     "name",
		PoolPropAltroot:  "altroot",
		PoolPropCapacity: "capacity",
	}

	// propMu protects the cached properties of datasets, which can be updated after a change.
	propMu sync.Mutex
)

// PoolOpen opens given pool
func (Adapter) PoolOpen(name string) (pool Pool, err error) {
	var names []string
	for _, n := range poolPropNames {
		names = append(names, n)
	}
	sort.Strings(names)

	out, err := run("zpool", "get", "-Hp", "-o", "property,value,source", strings.Join(names, ","), name)
	if err != nil {
		return pool, err
	}

	pool = Pool{Properties: make([]Property, PoolNumProps+1), name: name}
	for _, l := range splitLines(out) {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			return Pool{}, fmt.Errorf("unexpected zpool get output: %q", l)
		}
		for p, n := range poolPropNames {
			if n == f[0] {
				pool.Properties[p] = Property{Value: strings.Join(f[1:len(f)-1], "\t"), Source: nativeSource(f[len(f)-1])}
			}
		}
	}
	return pool, nil
}

// PoolCreate creates a zfs pool
func (a Adapter) PoolCreate(name string, vdev VDevTree, features map[string]string, props PoolProperties, fsprops DatasetProperties) (pool Pool, err error) {
	args := []string{"create", "-f"}
	for p, v := range props {
		n, ok := poolPropNames[p]
		if !ok || p == PoolPropName || p == PoolPropCapacity {
			return pool, fmt.Errorf("unsupported pool property %d on creation", p)
		}
		args = append(args, "-o", n+"="+v)
	}
	for f, v := range features {
		args = append(args, "-o", "feature@"+f+"="+v)
	}
	for p, v := range fsprops {
		n, ok := datasetPropNames[p]
		if !ok {
			return pool, fmt.Errorf("unsupported dataset property %d on pool creation", p)
		}
		args = append(args, "-O", n+"="+v)
	}
	args = append(args, name)
	devices := vdev.Devices
	if len(devices) == 0 {
		devices = []VDevTree{vdev}
	}
	for _, d := range devices {
		args = append(args, d.Path)
	}

	if _, err := run("zpool", args...); err != nil {
		return pool, err
	}
	return a.PoolOpen(name)
}

// Close releases the pool. Nothing is held between commands.
func (p Pool) Close() {}

// Export exports the pool.
func (p Pool) Export(force bool, log string) error {
	if p.name == "" {
		return errors.New("pool wasn't opened from ZFS")
	}
	args := []string{"export"}
	if force {
		args = append(args, "-f")
	}
	_, err := run("zpool", append(args, p.name)...)
	return err
}

// Destroy destroys the pool.
func (p Pool) Destroy(log string) error {
	if p.name == "" {
		return errors.New("pool wasn't opened from ZFS")
	}
	_, err := run("zpool", "destroy", "-f", p.name)
	return err
}

// DatasetOpenAll opens all the dataset recursively
func (Adapter) DatasetOpenAll() (datasets []DZFSInterface, err error) {
	all, err := readDatasets()
	if err != nil {
		return nil, err
	}

	var roots []string
	for n := range all {
		if !strings.ContainsAny(n, "/@") {
			roots = append(roots, n)
		}
	}
	sort.Strings(roots)

	children := childrenByParent(all)
	for _, r := range roots {
		d := buildTree(r, all, children)
		datasets = append(datasets, dZFSAdapter{&d})
	}
	return datasets, nil
}

// DatasetOpen opens a dataset and all its children
func (Adapter) DatasetOpen(name string) (DZFSInterface, error) {
	all, err := readDatasets("-r", name)
	if err != nil {
		return dZFSAdapter{}, err
	}
	if _, ok := all[name]; !ok {
		return dZFSAdapter{}, fmt.Errorf("no dataset found with name %q", name)
	}
	d := buildTree(name, all, childrenByParent(all))
	return dZFSAdapter{&d}, nil
}

// DatasetCreate creates a dataset without mounting it
func (a *Adapter) DatasetCreate(path string, dtype DatasetType, props map[Prop]Property) (DZFSInterface, error) {
	switch dtype {
	case DatasetTypeSnapshot:
		return a.DatasetSnapshot(path, false, props, nil)
	case DatasetTypeFilesystem, DatasetTypeVolume:
	default:
		return dZFSAdapter{}, fmt.Errorf("can't create dataset %q: unsupported type %d", path, dtype)
	}

	args := []string{"create"}
	if dtype == DatasetTypeVolume {
		size, ok := props[DatasetPropVolsize]
		if !ok {
			return dZFSAdapter{}, fmt.Errorf("can't create volume %q: no volume size", path)
		}
		args = append(args, "-V", size.Value)
	} else {
		args = append(args, "-u")
	}
	opts, err := propertiesToOptions(props, nil)
	if err != nil {
		return dZFSAdapter{}, err
	}
	args = append(append(args, opts...), path)

	if _, err := run("zfs", args...); err != nil {
		return dZFSAdapter{}, err
	}
	return a.DatasetOpen(path)
}

// DatasetSnapshot creates a snapshot
func (a *Adapter) DatasetSnapshot(path string, recur bool, props map[Prop]Property, userProps map[string]string) (DZFSInterface, error) {
	args := []string{"snapshot"}
	if recur {
		args = append(args, "-r")
	}
	opts, err := propertiesToOptions(props, userProps)
	if err != nil {
		return dZFSAdapter{}, err
	}
	args = append(append(args, opts...), path)

	if _, err := run("zfs", args...); err != nil {
		return dZFSAdapter{}, err
	}
	return a.DatasetOpen(path)
}

var seedOnce = sync.Once{}

// GenerateID returns a random string of the given length
func (*Adapter) GenerateID(length int) string {
	seedOnce.Do(func() { rand.Seed(time.Now().UnixNano()) })

	var allowedRunes = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

	b := make([]rune, length)
	for i := range b {
		b[i] = allowedRunes[rand.Intn(len(allowedRunes))]
	}
	return string(b)
}

type dZFSAdapter struct {
	*Dataset
}

func (d dZFSAdapter) name() string {
	return d.Dataset.Properties[DatasetPropName].Value
}

func (d dZFSAdapter) Children() (children []DZFSInterface) {
	for i := range d.Dataset.Children {
		children = append(children, dZFSAdapter{&d.Dataset.Children[i]})
	}
	return children
}

func (d dZFSAdapter) DZFSChildren() *[]Dataset {
	return &d.Dataset.Children
}

func (d dZFSAdapter) Properties() *map[Prop]Property {
	return &d.Dataset.Properties
}

func (d dZFSAdapter) Type() DatasetType {
	return d.Dataset.Type
}

func (d dZFSAdapter) Clone(target string, props map[Prop]Property) (DZFSInterface, error) {
	opts, err := propertiesToOptions(props, nil)
	if err != nil {
		return dZFSAdapter{}, err
	}
	args := append(append([]string{"clone"}, opts...), d.name(), target)
	if _, err := run("zfs", args...); err != nil {
		return dZFSAdapter{}, err
	}
	return Adapter{}.DatasetOpen(target)
}

// Clones returns the direct clones of a snapshot, or of all snapshots of a dataset, the most recent snapshots first.
func (d dZFSAdapter) Clones() (clones []string, err error) {
	if d.IsSnapshot() {
		out, err := run("zfs", "get", "-Hp", "-o", "value", "clones", d.name())
		if err != nil {
			return nil, err
		}
		return splitClones(strings.TrimSpace(out)), nil
	}

	out, err := run("zfs", "get", "-Hp", "-d", "1", "-t", "snapshot", "-o", "name,property,value", "createtxg,clones", d.name())
	if err != nil {
		return nil, err
	}
	type snap struct {
		txg    uint64
		clones []string
	}
	snaps := make(map[string]*snap)
	for _, l := range splitLines(out) {
		f := strings.Split(l, "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected zfs get output: %q", l)
		}
		s, ok := snaps[f[0]]
		if !ok {
			s = &snap{}
			snaps[f[0]] = s
		}
		switch f[1] {
		case "createtxg":
			if s.txg, err = strconv.ParseUint(f[2], 10, 64); err != nil {
				return nil, fmt.Errorf("invalid createtxg for %q: %v", f[0], err)
			}
		case "clones":
			s.clones = splitClones(f[2])
		}
	}
	var ordered []*snap
	for _, s := range snaps {
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].txg > ordered[j].txg })
	for _, s := range ordered {
		clones = append(clones, s.clones...)
	}
	return clones, nil
}

// Destroy destroys the dataset, which must have no children.
// Like libzfs, it refuses to destroy a mounted filesystem rather than unmounting it as the zfs command would.
func (d dZFSAdapter) Destroy(Defer bool) (err error) {
	if len(d.Dataset.Children) > 0 {
		return fmt.Errorf("cannot destroy dataset %s: it has children", d.name())
	}
	if !d.IsSnapshot() && d.Dataset.Type == DatasetTypeFilesystem {
		out, err := run("zfs", "get", "-Hp", "-o", "value", "mounted", d.name())
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "yes" {
			return fmt.Errorf("cannot destroy %s: dataset is busy", d.name())
		}
	}
	args := []string{"destroy"}
	if Defer {
		args = append(args, "-d")
	}
	_, err = run("zfs", append(args, d.name())...)
	return err
}

func (d dZFSAdapter) GetUserProperty(p string) (prop Property, err error) {
	propMu.Lock()
	prop, ok := d.Dataset.userProperties[p]
	propMu.Unlock()
	if ok {
		return prop, nil
	}

	// Not a scanned property: read and cache it.
	out, err := run("zfs", "get", "-Hp", "-o", "property,value,source", p, d.name())
	if err != nil {
		return prop, err
	}
	prop = Property{Value: "-", Source: "none"}
	for _, l := range splitLines(out) {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			return Property{}, fmt.Errorf("unexpected zfs get output: %q", l)
		}
		prop = userProperty(strings.Join(f[1:len(f)-1], "\t"), f[len(f)-1])
	}
	propMu.Lock()
	defer propMu.Unlock()
	if d.Dataset.userProperties == nil {
		d.Dataset.userProperties = make(map[string]Property)
	}
	d.Dataset.userProperties[p] = prop
	return prop, nil
}

func (d dZFSAdapter) Pool() (p Pool, err error) {
	return Adapter{}.PoolOpen(strings.FieldsFunc(d.name(), func(r rune) bool { return r == '/' || r == '@' })[0])
}

func (d dZFSAdapter) Promote() (err error) {
	if _, err := run("zfs", "promote", d.name()); err != nil {
		return err
	}
	return d.ReloadProperties()
}

// ReloadProperties reads again all properties of the dataset (but not of its children).
func (d dZFSAdapter) ReloadProperties() (err error) {
	all, err := readDatasets(d.name())
	if err != nil {
		return err
	}
	n, ok := all[d.name()]
	if !ok {
		return fmt.Errorf("no dataset found with name %q", d.name())
	}
	propMu.Lock()
	defer propMu.Unlock()
	d.Dataset.Type = n.Type
	d.Dataset.Properties = n.Properties
	d.Dataset.userProperties = n.userProperties
	d.Dataset.createTXG = n.createTXG
	return nil
}

func (d dZFSAdapter) SetUserProperty(prop, value string) error {
	if !strings.Contains(prop, ":") {
		return fmt.Errorf("%q isn't a user property", prop)
	}
	if _, err := run("zfs", "set", prop+"="+value, d.name()); err != nil {
		return err
	}
	propMu.Lock()
	defer propMu.Unlock()
	if d.Dataset.userProperties == nil {
		d.Dataset.userProperties = make(map[string]Property)
	}
	d.Dataset.userProperties[prop] = Property{Value: value, Source: "local"}
	return nil
}

func (d dZFSAdapter) SetProperty(p Prop, value string) error {
	n, ok := datasetPropNames[p]
	if !ok || p == DatasetPropName {
		return fmt.Errorf("unsupported property %d", p)
	}
	if _, err := run("zfs", "set", n+"="+value, d.name()); err != nil {
		return err
	}

	// Read back the property as set by ZFS
	out, err := run("zfs", "get", "-Hp", "-o", "property,value,source", n, d.name())
	if err != nil {
		return err
	}
	propMu.Lock()
	defer propMu.Unlock()
	delete(d.Dataset.Properties, p)
	for _, l := range splitLines(out) {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			return fmt.Errorf("unexpected zfs get output: %q", l)
		}
		if prop, ok := nativeProperty(f[0], strings.Join(f[1:len(f)-1], "\t"), f[len(f)-1]); ok {
			d.Dataset.Properties[p] = prop
		}
	}
	return nil
}

// readDatasets returns all filesystems, volumes and snapshots matching the given zfs get arguments, by name,
// with their scanned properties.
// Children are not attached.
func readDatasets(args ...string) (map[string]*Dataset, error) {
	out, err := run("zfs", append([]string{"get", "-Hp", "-t", "filesystem,volume,snapshot", "-o", "name,property,value,source", scannedProps}, args...)...)
	if err != nil {
		return nil, err
	}

	all := make(map[string]*Dataset)
	for _, l := range splitLines(out) {
		f := strings.Split(l, "\t")
		if len(f) < 4 {
			return nil, fmt.Errorf("unexpected zfs get output: %q", l)
		}
		name, prop, value, source := f[0], f[1], strings.Join(f[2:len(f)-1], "\t"), f[len(f)-1]

		d, ok := all[name]
		if !ok {
			d = &Dataset{
				Properties:     map[Prop]Property{DatasetPropName: {Value: name, Source: "none"}},
				userProperties: make(map[string]Property),
			}
			all[name] = d
		}

		switch {
		case prop == "type":
			switch value {
			case "filesystem":
				d.Type = DatasetTypeFilesystem
			case "volume":
				d.Type = DatasetTypeVolume
			case "snapshot":
				d.Type = DatasetTypeSnapshot
			case "bookmark":
				d.Type = DatasetTypeBookmark
			}
		case prop == "createtxg":
			if d.createTXG, err = strconv.ParseUint(value, 10, 64); err != nil {
				return nil, fmt.Errorf("invalid createtxg for %q: %v", name, err)
			}
		case strings.Contains(prop, ":"):
			d.userProperties[prop] = userProperty(value, source)
		default:
			p, ok := datasetPropsByName[prop]
			if !ok {
				continue
			}
			if np, ok := nativeProperty(prop, value, source); ok {
				d.Properties[p] = np
			}
		}
	}
	return all, nil
}

// childrenByParent returns the names of direct children of each dataset: filesystems and volumes
// by name first, then snapshots by creation order, as libzfs iterates them.
func childrenByParent(all map[string]*Dataset) map[string][]string {
	children := make(map[string][]string)
	for n := range all {
		var parent string
		if i := strings.LastIndex(n, "@"); i >= 0 {
			parent = n[:i]
		} else if i := strings.LastIndex(n, "/"); i >= 0 {
			parent = n[:i]
		} else {
			continue
		}
		if _, ok := all[parent]; !ok {
			continue
		}
		children[parent] = append(children[parent], n)
	}
	for _, c := range children {
		sort.Slice(c, func(i, j int) bool {
			si, sj := strings.Contains(c[i], "@"), strings.Contains(c[j], "@")
			if si != sj {
				return sj
			}
			if si && all[c[i]].createTXG != all[c[j]].createTXG {
				return all[c[i]].createTXG < all[c[j]].createTXG
			}
			return c[i] < c[j]
		})
	}
	return children
}

func buildTree(name string, all map[string]*Dataset, children map[string][]string) Dataset {
	d := *all[name]
	d.Children = nil
	for _, c := range children[name] {
		d.Children = append(d.Children, buildTree(c, all, children))
	}
	return d
}

// nativeProperty converts zfs get output to a property as libzfs reports it.
// It returns false for a property without a value.
func nativeProperty(prop, value, source string) (Property, bool) {
	// Not valid for this dataset type, or not a clone for origin
	if value == "-" && (source == "-" || prop == "origin") {
		return Property{}, false
	}
	return Property{Value: value, Source: nativeSource(source)}, true
}

// nativeSource converts a zfs get source to the libzfs source name.
func nativeSource(source string) string {
	switch {
	case source == "-":
		return "none"
	case strings.HasPrefix(source, "inherited from "):
		return "inherited"
	}
	return source
}

// userProperty converts zfs get output to a user property as libzfs reports it:
// the source of an inherited property is the dataset it is inherited from.
func userProperty(value, source string) Property {
	switch {
	case source == "-":
		source = "none"
	case strings.HasPrefix(source, "inherited from "):
		source = strings.TrimPrefix(source, "inherited from ")
	}
	return Property{Value: value, Source: source}
}

// propertiesToOptions returns the -o options to set the given properties on creation.
// Read only properties are skipped.
func propertiesToOptions(props map[Prop]Property, userProps map[string]string) ([]string, error) {
	var opts []string
	for p, v := range props {
		switch p {
		case DatasetPropName, DatasetPropOrigin, DatasetPropMounted, DatasetPropCreation, DatasetPropVolsize:
			continue
		}
		n, ok := datasetPropNames[p]
		if !ok {
			return nil, fmt.Errorf("unsupported property %d", p)
		}
		opts = append(opts, n+"="+v.Value)
	}
	for p, v := range userProps {
		opts = append(opts, p+"="+v)
	}
	sort.Strings(opts)

	var args []string
	for _, o := range opts {
		args = append(args, "-o", o)
	}
	return args, nil
}

func splitClones(v string) []string {
	if v == "" || v == "-" {
		return nil
	}
	return strings.Split(v, ",")
}

func splitLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// run executes a zfs or zpool command and returns its standard output.
func run(command string, args ...string) (string, error) {
	cmd := exec.Command(commandPath(command), args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s failed: %v: %s", command, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// commandPath finds a command in PATH, then in the sbin directories which might be missing from PATH.
func commandPath(command string) string {
	if p, err := exec.LookPath(command); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		p := filepath.Join(dir, command)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return command
}
