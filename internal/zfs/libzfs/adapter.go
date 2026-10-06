package libzfs

// Prop enumerates the native ZFS properties zsys reads or sets.
// Values are internal to zsys and are translated to property names when talking to ZFS.
type Prop int

// Property is a ZFS pool or dataset property value with its source.
type Property struct {
	Value  string
	Source string
}

// PoolProperties maps pool properties to values.
type PoolProperties map[Prop]string

// DatasetProperties maps dataset properties to values.
type DatasetProperties map[Prop]string

// VDevType is the type of a virtual device.
type VDevType string

// VDevTree describes the virtual devices of a pool to create.
type VDevTree struct {
	Type    VDevType
	Devices []VDevTree
	Path    string
}

// DatasetType is the type of a dataset.
type DatasetType int

// Pool is a ZFS pool with the properties zsys uses.
type Pool struct {
	Properties []Property
	name       string
}

// Dataset is a ZFS dataset with the properties zsys uses and its children.
type Dataset struct {
	Type       DatasetType
	Properties map[Prop]Property
	Children   []Dataset

	userProperties map[string]Property
	createTXG      uint64
}

// IsSnapshot returns true if the dataset is a snapshot
func (d *Dataset) IsSnapshot() bool {
	return d.Type == DatasetTypeSnapshot
}

// Close releases the dataset. Nothing is held between commands.
func (d *Dataset) Close() {}

const (
	// DatasetPropType is never read nor set: it keeps the zero value of Prop meaning "no native property".
	DatasetPropType Prop = iota
	// DatasetPropName is the name of the dataset
	DatasetPropName
	// DatasetPropCanmount is the canmount property of the dataset
	DatasetPropCanmount
	// DatasetPropMountpoint is the mountpoint of the dataset
	DatasetPropMountpoint
	// DatasetPropOrigin is the origin of the dataset
	DatasetPropOrigin
	// DatasetPropMounted is the mounted property for the dataset
	DatasetPropMounted
	// DatasetPropCreation is the creation time property for the dataset
	DatasetPropCreation
	// DatasetPropVolsize is the volume size property for the dataset
	DatasetPropVolsize
)

const (
	// PoolPropName is the name of the pool
	PoolPropName Prop = iota
	// PoolPropAltroot ZFS Pool property
	PoolPropAltroot
	// PoolPropCapacity ZFS Pool property
	PoolPropCapacity
	// PoolNumProps is the end pool number property
	PoolNumProps
)

const (
	// VDevTypeFile is the vdevtype on file
	VDevTypeFile VDevType = "file"
)

const (
	// DatasetTypeFilesystem - file system dataset
	DatasetTypeFilesystem DatasetType = 1 << iota
	// DatasetTypeSnapshot - snapshot of dataset
	DatasetTypeSnapshot
	// DatasetTypeVolume - volume (virtual block device) dataset
	DatasetTypeVolume
	// DatasetTypePool - pool dataset
	DatasetTypePool
	// DatasetTypeBookmark - bookmark dataset
	DatasetTypeBookmark
)

const (
	zsysPrefix = "com.ubuntu.zsys:"
	// BootfsProp string value
	BootfsProp = zsysPrefix + "bootfs"
	// LastUsedProp string value
	LastUsedProp = zsysPrefix + "last-used"
	// BootfsDatasetsProp string value
	BootfsDatasetsProp = zsysPrefix + "bootfs-datasets"
	// LastBootedKernelProp string value
	LastBootedKernelProp = zsysPrefix + "last-booted-kernel"
	// CanmountProp string value
	CanmountProp = "canmount"
	// SnapshotCanmountProp is the equivalent to CanmountProp, but as a user property to store on zsys snapshot
	SnapshotCanmountProp = zsysPrefix + CanmountProp
	// MountPointProp string value
	MountPointProp = "mountpoint"
	// SnapshotMountpointProp is the equivalent to MountPointProp, but as a user property to store on zsys snapshot
	SnapshotMountpointProp = zsysPrefix + MountPointProp
)

// Interface is the interface to use real libzfs or our in memory mock.
type Interface interface {
	PoolOpen(name string) (pool Pool, err error)
	DatasetOpenAll() (datasets []DZFSInterface, err error)
	DatasetOpen(name string) (d DZFSInterface, err error)
	DatasetCreate(path string, dtype DatasetType, props map[Prop]Property) (d DZFSInterface, err error)
	DatasetSnapshot(path string, recur bool, props map[Prop]Property, userProps map[string]string) (rd DZFSInterface, err error)
	GenerateID(length int) string
}

// DZFSInterface is the interface to use real libzfs Dataset object or in memory mock.
type DZFSInterface interface {
	DZFSChildren() *[]Dataset
	Children() []DZFSInterface
	Clone(target string, props map[Prop]Property) (rd DZFSInterface, err error)
	Clones() (clones []string, err error)
	Close()
	Destroy(Defer bool) (err error)
	GetUserProperty(p string) (prop Property, err error)
	IsSnapshot() (ok bool)
	Pool() (p Pool, err error)
	Promote() (err error)
	Properties() *map[Prop]Property
	ReloadProperties() (err error)
	SetUserProperty(prop, value string) error
	SetProperty(p Prop, value string) error
	Type() DatasetType
}
