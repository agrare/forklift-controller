package vsphere

import (
	libmodel "github.com/konveyor/controller/pkg/inventory/model"
	"github.com/konveyor/forklift-controller/pkg/controller/provider/model/base"
	"strings"
)

//
// Networks (variant).
const (
	NetStandard    = "Standard"
	NetDvPortGroup = "DvPortGroup"
	NetDvSwitch    = "DvSwitch"
)

//
// Errors
var NotFound = libmodel.NotFound

type InvalidRefError = base.InvalidRefError

const (
	MaxDetail = base.MaxDetail
)

//
// Types
type Model = base.Model
type ListOptions = base.ListOptions
type Concern = base.Concern
type Ref = base.Ref

//
// Base VMWare model.
type Base struct {
	// Managed object ID.
	ID string `sql:"pk"`
	// Name
	Name string `sql:"d0,index(name)"`
	// Parent
	Parent Ref `sql:"d0,index(parent)"`
	// Revision
	Revision int64 `sql:"incremented,d0,index(revision)"`
}

//
// Get the PK.
func (m *Base) Pk() string {
	return m.ID
}

//
// String representation.
func (m *Base) String() string {
	return m.ID
}

//
// Get labels.
func (m *Base) Labels() libmodel.Labels {
	return nil
}

func (m *Base) Equals(other libmodel.Model) bool {
	if vm, cast := other.(*VM); cast {
		return m.ID == vm.ID
	}

	return false
}

//
// Populate PK using the ref.
func (m *Base) WithRef(ref Ref) {
	m.ID = ref.ID
}

// Determine object path.
func (m *Base) Path(db libmodel.DB) (path string, err error) {
	parts := []string{m.Name}
	node := m
Walk:
	for {
		parent := node.Parent
		switch parent.Kind {
		case FolderKind:
			f := &Folder{}
			f.WithRef(parent)
			err = db.Get(f)
			if err != nil {
				return
			}
			parts = append(parts, f.Name)
			node = &f.Base
		case DatacenterKind:
			m := &Datacenter{}
			m.WithRef(parent)
			err = db.Get(m)
			if err != nil {
				return
			}
			parts = append(parts, m.Name)
			node = &m.Base
			break Walk
		case ClusterKind:
			m := &Cluster{}
			m.WithRef(parent)
			err = db.Get(m)
			if err != nil {
				return
			}
			parts = append(parts, m.Name)
			node = &m.Base
		case HostKind:
			m := &Host{}
			m.WithRef(parent)
			err = db.Get(m)
			if err != nil {
				return
			}
			parts = append(parts, m.Name)
			node = &m.Base
		case NetKind:
			m := &Network{}
			m.WithRef(parent)
			err = db.Get(m)
			if err != nil {
				return
			}
			parts = append(parts, m.Name)
			node = &m.Base
		case DsKind:
			m := &Datastore{}
			m.WithRef(parent)
			err = db.Get(m)
			if err != nil {
				return
			}
			parts = append(parts, m.Name)
			node = &m.Base
		default:
			break Walk
		}
	}

	reversed := []string{""}
	for i := len(parts) - 1; i >= 0; i-- {
		reversed = append(reversed, parts[i])
	}

	path = strings.Join(reversed, "/")

	return
}

type About struct {
	Base
	APIVersion string `sql:""`
	Product    string `sql:""`
}

type Folder struct {
	Base
	Datacenter string `sql:"d0,index(datacenter)"`
	Folder     string `sql:"d0,index(folder)"`
	Children   []Ref  `sql:""`
}

type Datacenter struct {
	Base
	Clusters   Ref `sql:""`
	Networks   Ref `sql:""`
	Datastores Ref `sql:""`
	Vms        Ref `sql:""`
}

type Cluster struct {
	Base
	Folder      string `sql:"d0,index(folder)"`
	Hosts       []Ref  `sql:""`
	Networks    []Ref  `sql:""`
	Datastores  []Ref  `sql:""`
	DasEnabled  bool   `sql:""`
	DasVms      []Ref  `sql:""`
	DrsEnabled  bool   `sql:""`
	DrsBehavior string `sql:""`
	DrsVms      []Ref  `sql:""`
}

type Host struct {
	Base
	Cluster            string      `sql:"d0,index(cluster)"`
	InMaintenanceMode  bool        `sql:""`
	ManagementServerIp string      `sql:""`
	Thumbprint         string      `sql:""`
	CpuSockets         int16       `sql:""`
	CpuCores           int16       `sql:""`
	ProductName        string      `sql:""`
	ProductVersion     string      `sql:""`
	Network            HostNetwork `sql:""`
	Networks           []Ref       `sql:""`
	Datastores         []Ref       `sql:""`
	Vms                []Ref       `sql:""`
}

type HostNetwork struct {
	PNICs      []PNIC      `json:"pNICs"`
	VNICs      []VNIC      `json:"vNICs"`
	PortGroups []PortGroup `json:"portGroups"`
	Switches   []Switch    `json:"switches"`
}

func (n *HostNetwork) Switch(key string) (vSwitch *Switch, found bool) {
	for _, object := range n.Switches {
		if key == object.Key {
			vSwitch = &object
			found = true
			break
		}
	}

	return
}
func (n *HostNetwork) PortGroup(name string) (portGroup *PortGroup, found bool) {
	for _, object := range n.PortGroups {
		if name == object.Name {
			portGroup = &object
			found = true
			break
		}
	}

	return
}

func (n *HostNetwork) PNIC(key string) (nic *PNIC, found bool) {
	for _, object := range n.PNICs {
		if key == object.Key {
			nic = &object
			found = true
			break
		}
	}

	return
}

type PNIC struct {
	Key       string `json:"key"`
	LinkSpeed int32  `json:"linkSpeed"`
}

type VNIC struct {
	Key        string `json:"key"`
	PortGroup  string `json:"portGroup"`
	DPortGroup string `json:"dPortGroup"`
	IpAddress  string `json:"ipAddress"`
	SubnetMask string `json:"subnetMask"`
	MTU        int32  `json:"mtu"`
}

type PortGroup struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Switch string `json:"vSwitch"`
}

type Switch struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	PortGroups []string `json:"portGroups"`
	PNICs      []string `json:"pNICs"`
}

type Network struct {
	Base
	Variant  string    `sql:"d0"`
	Tag      string    `sql:""`
	DVSwitch Ref       `sql:""`
	Host     []DVSHost `sql:""`
}

type DVSHost struct {
	Host Ref
	PNIC []string
}

type Datastore struct {
	Base
	Type            string `sql:""`
	Capacity        int64  `sql:""`
	Free            int64  `sql:""`
	MaintenanceMode string `sql:""`
}

type VM struct {
	Base
	Folder                string    `sql:"d0,index(folder)"`
	Host                  string    `sql:"d0,index(host)"`
	RevisionValidated     int64     `sql:"d0,index(revisionValidated)"`
	PolicyVersion         int       `sql:"d0,index(policyVersion)"`
	UUID                  string    `sql:""`
	Firmware              string    `sql:""`
	PowerState            string    `sql:""`
	ConnectionState       string    `sql:""`
	CpuAffinity           []int32   `sql:""`
	CpuHotAddEnabled      bool      `sql:""`
	CpuHotRemoveEnabled   bool      `sql:""`
	MemoryHotAddEnabled   bool      `sql:""`
	FaultToleranceEnabled bool      `sql:""`
	CpuCount              int32     `sql:""`
	CoresPerSocket        int32     `sql:""`
	MemoryMB              int32     `sql:""`
	GuestName             string    `sql:""`
	BalloonedMemory       int32     `sql:""`
	IpAddress             string    `sql:""`
	NumaNodeAffinity      []string  `sql:""`
	StorageUsed           int64     `sql:""`
	Snapshot              Ref       `sql:""`
	IsTemplate            bool      `sql:""`
	ChangeTrackingEnabled bool      `sql:""`
	Devices               []Device  `sql:""`
	Disks                 []Disk    `sql:""`
	Networks              []Ref     `sql:""`
	Concerns              []Concern `sql:""`
}

//
// Determine if current revision has been validated.
func (m *VM) Validated() bool {
	return m.RevisionValidated == m.Revision
}

//
// Virtual Disk.
type Disk struct {
	File      string `json:"file"`
	Datastore Ref    `json:"datastore"`
	Capacity  int64  `json:"capacity"`
	Shared    bool   `json:"shared"`
	RDM       bool   `json:"rdm"`
}

//
// Virtual Device.
type Device struct {
	Kind string `json:"kind"`
}
