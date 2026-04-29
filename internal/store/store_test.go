package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreNewEmpty(t *testing.T) {
	t.Run("nonexistent path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "does-not-exist.json")
		st, err := New(path)
		if err != nil {
			t.Fatalf("New on nonexistent path: %v", err)
		}
		if st.Get().Vacuums == nil {
			t.Fatalf("Vacuums map should be non-nil")
		}
		if len(st.Get().Vacuums) != 0 {
			t.Fatalf("Vacuums map should be empty, got %d", len(st.Get().Vacuums))
		}
	})

	t.Run("bad JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(path); err == nil {
			t.Fatal("expected error on bad JSON, got nil")
		}
	})
}

func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.SetAlarmState("ARMED_HOME"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetControlState("running"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVacuumStartedByUs("192.168.1.10", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVerisureCookie("cookie-value"); err != nil {
		t.Fatal(err)
	}

	st2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.Get()
	if got.AlarmState != "ARMED_HOME" {
		t.Errorf("AlarmState = %q, want ARMED_HOME", got.AlarmState)
	}
	if got.ControlState != "running" {
		t.Errorf("ControlState = %q, want running", got.ControlState)
	}
	if got.VerisureCookie != "cookie-value" {
		t.Errorf("VerisureCookie = %q, want cookie-value", got.VerisureCookie)
	}
	v, ok := got.Vacuums["192.168.1.10"]
	if !ok {
		t.Fatalf("vacuum entry missing")
	}
	if !v.StartedByUs {
		t.Errorf("StartedByUs = false, want true")
	}
}

func TestStoreAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlarmState("DISARMED"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not exist after successful write, err=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		t.Errorf("written file is not valid JSON: %v", err)
	}
}

func TestStoreGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVacuumStartedByUs("host1", true); err != nil {
		t.Fatal(err)
	}

	t.Run("returns copy", func(t *testing.T) {
		got := st.Get()
		got.AlarmState = "MUTATED"
		got.Vacuums["host1"] = VacuumEntry{Name: "MUTATED"}
		got.Vacuums["new"] = VacuumEntry{}

		again := st.Get()
		if again.AlarmState == "MUTATED" {
			t.Error("mutating returned State affected store AlarmState")
		}
		if again.Vacuums["host1"].Name == "MUTATED" {
			t.Error("mutating returned Vacuums map affected store")
		}
		if _, ok := again.Vacuums["new"]; ok {
			t.Error("adding to returned map affected store")
		}
	})

	t.Run("empty vacuums non-nil", func(t *testing.T) {
		dir := t.TempDir()
		fresh, err := New(filepath.Join(dir, "x.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Get().Vacuums == nil {
			t.Error("Vacuums map nil on fresh store")
		}
	})
}

func TestStoreMultiVacuum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetVacuumStartedByUs("host-a", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVacuumStartedByUs("host-b", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVacuumName("host-a", "kitchen"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetVacuumName("host-b", "living"); err != nil {
		t.Fatal(err)
	}

	st2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.Get()
	a, ok := got.Vacuums["host-a"]
	if !ok || !a.StartedByUs || a.Name != "kitchen" {
		t.Errorf("host-a entry wrong: %+v ok=%v", a, ok)
	}
	b, ok := got.Vacuums["host-b"]
	if !ok || b.StartedByUs || b.Name != "living" {
		t.Errorf("host-b entry wrong: %+v ok=%v", b, ok)
	}
}

func TestStoreConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := st.SetAlarmState("state"); err != nil {
				t.Errorf("SetAlarmState: %v", err)
			}
		}(i)
	}
	wg.Wait()
}
