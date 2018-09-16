package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/parsers"
	"github.com/pkg/errors"

	"github.com/docker/go-plugins-helpers/volume"
	"golang.org/x/sys/unix"
)

const (
	driverName = "tmpsync"
)

type tmpsyncVolume struct {
	Options []string

	Mountpoint string
	FsSize     string
	Target     string
	OpMode     string
	SshKey     string
}

type tmpsyncOptions struct {
	RootPath string
}

type tmpsyncDriver struct {
	options tmpsyncOptions

	sync.RWMutex

	volumes map[string]*tmpsyncVolume
}

func parseOptions(options []string) (*tmpsyncOptions, error) {
	opts := &tmpsyncOptions{}
	for _, opt := range options {
		key, val, err := parsers.ParseKeyValueOpt(opt)
		if err != nil {
			return nil, err
		}
		key = strings.ToLower(key)
		switch key {
		case "root":
			opts.RootPath, _ = filepath.Abs(val)
		default:
			return nil, errors.Errorf("tmpsync: unknown option (%s = %s)", key, val)
		}
	}

	return opts, nil
}

func (d *tmpsyncDriver) getMntPath(name string) string {
	return path.Join(d.options.RootPath, name)
}

func (d *tmpsyncDriver) execRsyncCommand(target, source, opmode, sshkey string) error {
	args := []string{}

	if strings.Contains(opmode, "archive") {
		args = append(args, "--archive")
	}
	if strings.Contains(opmode, "compress") {
		args = append(args, "--compress")
	}
	if strings.Contains(opmode, "delete") {
		args = append(args, "--delete")
	}
	if strings.Contains(opmode, "recursive") {
		args = append(args, "--recursive")
	}
	if sshkey != "" {
		args = append(args, "-e")
		args = append(args, fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=quiet -i %v", sshkey))
	}

	args = append(args, source)
	args = append(args, target)

	if out, err := exec.Command("rsync", args...).CombinedOutput(); err != nil {
		log.Println(string(out))
		return err
	}

	return nil
}

func (d *tmpsyncDriver) Create(r *volume.CreateRequest) error {
	log.Printf("tmpsync: create (%v)\n", r)

	d.Lock()
	defer d.Unlock()

	v := &tmpsyncVolume{}
	v.Mountpoint = d.getMntPath(r.Name)

	if err := os.MkdirAll(v.Mountpoint, 0755); err != nil {
		return err
	}

	for key, val := range r.Options {
		switch key {
		case "fssize":
			v.FsSize = val
		case "target":
			v.Target = val
		case "opmode":
			v.OpMode = val
		case "sshkey":
			v.SshKey = val
		default:
			return errors.Errorf("tmpsync: unknown option (%s = %s)", key, val)
		}
	}

	d.volumes[r.Name] = v

	return nil
}

func (d *tmpsyncDriver) Remove(r *volume.RemoveRequest) error {
	log.Printf("tmpsync: remove (%v)\n", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return errors.Errorf("tmpsync: volume %s not found", r.Name)
	}

	if err := os.RemoveAll(v.Mountpoint); err != nil {
		return err
	}

	delete(d.volumes, r.Name)

	return nil
}

func (d *tmpsyncDriver) Path(r *volume.PathRequest) (*volume.PathResponse, error) {
	log.Printf("tmpsync: path (%v)\n", r)

	d.RLock()
	defer d.RUnlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.PathResponse{}, errors.Errorf("tmpsync: volume %s not found", r.Name)
	}

	return &volume.PathResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *tmpsyncDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	log.Printf("tmpsync: mount (%v)\n", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.MountResponse{}, errors.Errorf("tmpsync: volume %s not found", r.Name)
	}

	if err := unix.Mount("tmpfs", v.Mountpoint, "tmpfs", 0, fmt.Sprintf("size=%v", v.FsSize)); err != nil {
		return &volume.MountResponse{}, errors.Errorf("tmpsync: could not mount tmpfs on %v", r.Name)
	}

	return &volume.MountResponse{
		Mountpoint: v.Mountpoint,
	}, nil
}

func (d *tmpsyncDriver) Unmount(r *volume.UnmountRequest) error {
	log.Printf("tmpsync: unmount (%v)\n", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return errors.Errorf("tmpsync: volume %s not found", r.Name)
	}

	if err := d.execRsyncCommand(v.Target, v.Mountpoint, v.OpMode, v.SshKey); err != nil {
		return errors.Errorf("tmpsync: could not rsync tmpfs on %v", r.Name)
	}

	mount.RecursiveUnmount(v.Mountpoint)

	return nil
}

func (d *tmpsyncDriver) Get(r *volume.GetRequest) (*volume.GetResponse, error) {
	log.Printf("tmpsync: get (%v)\n", r)

	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.GetResponse{}, errors.Errorf("tmpsync: volume %s not found", r.Name)
	}

	return &volume.GetResponse{
		Volume: &volume.Volume{
			Name:       r.Name,
			Mountpoint: v.Mountpoint,
			CreatedAt:  time.Now().Format(time.RFC3339),
		},
	}, nil
}

func (d *tmpsyncDriver) List() (*volume.ListResponse, error) {
	log.Printf("tmpsync: list ()\n")

	d.Lock()
	defer d.Unlock()

	var volumes []*volume.Volume
	for name, v := range d.volumes {
		volumes = append(volumes, &volume.Volume{Name: name, Mountpoint: v.Mountpoint})
	}

	return &volume.ListResponse{
		Volumes: volumes,
	}, nil
}

func (d *tmpsyncDriver) Capabilities() *volume.CapabilitiesResponse {
	log.Printf("tmpsync: capabilities ()\n")

	return &volume.CapabilitiesResponse{
		Capabilities: volume.Capability{
			Scope: "local",
		},
	}
}

func NewTmpsyncDriver(options []string) (*tmpsyncDriver, error) {
	log.Printf("tmpsync: createDriver (%v)\n", options)

	opts, err := parseOptions(options)
	if err != nil {
		return nil, err
	}

	d := &tmpsyncDriver{
		volumes: map[string]*tmpsyncVolume{},
	}
	d.options = *opts

	return d, nil
}
