package cncd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/filebrowser/filebrowser/v2/settings"
)

func TestLoadStore_CreatesFileOnFirstBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cncd.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: expected %s to not exist", path)
	}

	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected LoadStore to create %s: %v", path, err)
	}
	if got := store.Snapshot(); len(got.Machines) != 0 {
		t.Fatalf("expected zero-value config, got %+v", got)
	}
}

func TestStore_UpdatePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cncd.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	err = store.Update(func(c *settings.Cnc) error {
		c.MachineToken = "tok-1"
		c.BaselinePollSeconds = 30
		c.Machines = []settings.Machine{{ID: "m1", Name: "Mill", Host: "10.0.0.5", Port: 4196}}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload LoadStore: %v", err)
	}
	got := reloaded.Snapshot()
	if got.MachineToken != "tok-1" {
		t.Errorf("MachineToken = %q, want tok-1", got.MachineToken)
	}
	if got.BaselinePollSeconds != 30 {
		t.Errorf("BaselinePollSeconds = %d, want 30", got.BaselinePollSeconds)
	}
	if len(got.Machines) != 1 || got.Machines[0].ID != "m1" {
		t.Errorf("Machines = %+v, want one machine m1", got.Machines)
	}
}

func TestStore_Get_WrapsForCncPackage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cncd.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := store.Update(func(c *settings.Cnc) error {
		c.Machines = []settings.Machine{{ID: "m1"}}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	set, err := store.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(set.Cnc.Machines) != 1 || set.Cnc.Machines[0].ID != "m1" {
		t.Fatalf("Get().Cnc.Machines = %+v, want one machine m1", set.Cnc.Machines)
	}
}

func TestStore_UpdateErrorDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cncd.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	sentinel := os.ErrInvalid
	err = store.Update(func(c *settings.Cnc) error {
		c.MachineToken = "should-not-persist"
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("Update err = %v, want sentinel", err)
	}

	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Snapshot().MachineToken != "" {
		t.Fatalf("expected on-disk file to be untouched by a failed Update")
	}
}
