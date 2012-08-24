// relation implements persistent local storage of a unit's relation state, and
// translation of relation changes into hooks that need to be run.
package relation

import (
	"fmt"
	"io/ioutil"
	"launchpad.net/juju-core/trivial"
	"launchpad.net/juju-core/worker/uniter/hook"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// State describes the state of a relation.
type State struct {
	// RelationId identifies the relation.
	RelationId int

	// Members is a map from unit name to the last change version
	// for which a hook.Info was delivered on the output channel.
	Members map[string]int

	// ChangedPending indicates that a "relation-changed" hook for the given
	// unit name must be the first hook.Info to be sent to the output channel.
	ChangedPending string
}

// copy returns an independent copy of the state.
func (s *State) copy() *State {
	copy := &State{
		RelationId:     s.RelationId,
		ChangedPending: s.ChangedPending,
	}
	if s.Members != nil {
		copy.Members = map[string]int{}
		for m, v := range s.Members {
			copy.Members[m] = v
		}
	}
	return copy
}

// Validate returns an error if the supplied hook.Info does not represent
// a valid change to the relation state. Hooks must always be validated
// against the current state before they are run, to ensure that the system
// meets its guarantees about hook execution order.
func (s *State) Validate(hi hook.Info) (err error) {
	defer trivial.ErrorContextf(&err, "inappropriate %q for %q", hi.Kind, hi.RemoteUnit)
	if hi.RelationId != s.RelationId {
		return fmt.Errorf("expected relation %d, got relation %d", s.RelationId, hi.RelationId)
	}
	if s.Members == nil {
		return fmt.Errorf(`relation is broken and cannot be changed further`)
	}
	unit, kind := hi.RemoteUnit, hi.Kind
	if kind == hook.RelationBroken {
		if len(s.Members) == 0 {
			return nil
		}
		return fmt.Errorf(`cannot run "relation-broken" while units still present`)
	}
	if s.ChangedPending != "" {
		if unit != s.ChangedPending || kind != hook.RelationChanged {
			return fmt.Errorf(`expected "relation-changed" for %q`, s.ChangedPending)
		}
	} else if _, joined := s.Members[unit]; joined && kind == hook.RelationJoined {
		return fmt.Errorf("unit already joined")
	} else if !joined && kind != hook.RelationJoined {
		return fmt.Errorf("unit has not joined")
	}
	return nil
}

// StateDir is a filesystem-backed representation of the state of a
// relation. Concurrent modifications to the underlying state directory
// will have undefined consequences.
type StateDir struct {
	// path identifies the directory holding persistent state.
	path string

	// state is the cached state of the directory, which is guaranteed
	// to be synchronized with the true state so long as no concurrent
	// changes are made to the directory.
	state State
}

// State returns the current state of the relation.
func (d *StateDir) State() *State {
	return d.state.copy()
}

// ReadStateDir loads a StateDir from the subdirectory of dirPath named
// for the supplied RelationId. If the directory does not exist, no error
// is returned,
func ReadStateDir(dirPath string, relationId int) (d *StateDir, err error) {
	d = &StateDir{
		filepath.Join(dirPath, strconv.Itoa(relationId)),
		State{relationId, map[string]int{}, ""},
	}
	defer trivial.ErrorContextf(&err, "cannot load relation state from %q", d.path)
	if _, err := os.Stat(d.path); os.IsNotExist(err) {
		return d, nil
	} else if err != nil {
		return nil, err
	}
	fis, err := ioutil.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	for _, fi := range fis {
		// Entries with names ending in "-" followed by an integer must be
		// files containing valid unit data; all other names are ignored.
		name := fi.Name()
		i := strings.LastIndex(name, "-")
		if i == -1 {
			continue
		}
		svcName := name[:i]
		unitId := name[i+1:]
		if _, err := strconv.Atoi(unitId); err != nil {
			continue
		}
		unitName := svcName + "/" + unitId
		var info diskInfo
		if err = trivial.ReadYaml(filepath.Join(d.path, name), &info); err != nil {
			return nil, fmt.Errorf("invalid unit file %q: %v", name, err)
		}
		if info.ChangeVersion == nil {
			return nil, fmt.Errorf(`invalid unit file %q: "changed-version" not set`, name)
		}
		d.state.Members[unitName] = *info.ChangeVersion
		if info.ChangedPending {
			if d.state.ChangedPending != "" {
				return nil, fmt.Errorf("%q and %q both have pending changed hooks", d.state.ChangedPending, unitName)
			}
			d.state.ChangedPending = unitName
		}
	}
	return d, nil
}

// ReadAllStateDirs loads and returns every StateDir persisted directly inside
// the supplied dirPath. If dirPath does not exist, no error is returned.
func ReadAllStateDirs(dirPath string) (dirs map[int]*StateDir, err error) {
	defer trivial.ErrorContextf(&err, "cannot load relations state from %q", dirPath)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	fis, err := ioutil.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}
	dirs = map[int]*StateDir{}
	for _, fi := range fis {
		// Entries with integer names must be directories containing StateDir
		// data; all other names will be ignored.
		relationId, err := strconv.Atoi(fi.Name())
		if err != nil {
			// This doesn't look like a relation.
			continue
		}
		dir, err := ReadStateDir(dirPath, relationId)
		if err != nil {
			return nil, err
		}
		dirs[relationId] = dir
	}
	return dirs, nil
}

// Ensure creates the directory if it does not already exist.
func (d *StateDir) Ensure() error {
	return trivial.EnsureDir(d.path)
}

// Write atomically writes to disk the relation state change in hi.
// It must be called after the respective hook was executed successfully.
// Write doesn't validate hi but guarantees that successive writes of
// the same hi are idempotent.
func (d *StateDir) Write(hi hook.Info) (err error) {
	defer trivial.ErrorContextf(&err, "failed to write %q hook info for %q on state directory", hi.Kind, hi.RemoteUnit)
	if hi.Kind == hook.RelationBroken {
		if err = os.Remove(d.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		// If atomic delete succeeded, update own state.
		d.state.Members = nil
		return nil
	}
	name := strings.Replace(hi.RemoteUnit, "/", "-", 1)
	path := filepath.Join(d.path, name)
	if hi.Kind == hook.RelationDeparted {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		// If atomic delete succeeded, update own state.
		delete(d.state.Members, hi.RemoteUnit)
		return nil
	}
	di := diskInfo{&hi.ChangeVersion, hi.Kind == hook.RelationJoined}
	if err := trivial.WriteYaml(path, &di); err != nil {
		return err
	}
	// If write was successful, update own state.
	d.state.Members[hi.RemoteUnit] = hi.ChangeVersion
	if hi.Kind == hook.RelationJoined {
		d.state.ChangedPending = hi.RemoteUnit
	} else {
		d.state.ChangedPending = ""
	}
	return nil
}

// diskInfo defines the relation unit data serialization.
type diskInfo struct {
	ChangeVersion  *int `yaml:"change-version"`
	ChangedPending bool `yaml:"changed-pending,omitempty"`
}