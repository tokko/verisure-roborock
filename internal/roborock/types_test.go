package roborock

import (
	"encoding/json"
	"testing"
	"time"
)

func TestVacuumStatePredicates(t *testing.T) {
	t.Run("IsActiveClean", func(t *testing.T) {
		active := []VacuumStateID{5, 11, 17, 18}
		for _, s := range active {
			if !s.IsActiveClean() {
				t.Errorf("state %d: IsActiveClean=false, want true", s)
			}
		}
		for s := VacuumStateID(1); s <= 20; s++ {
			isExpected := false
			for _, a := range active {
				if a == s {
					isExpected = true
					break
				}
			}
			if !isExpected && s.IsActiveClean() {
				t.Errorf("state %d: IsActiveClean=true, want false", s)
			}
		}
	})

	t.Run("IsPaused", func(t *testing.T) {
		if !VacuumStateID(10).IsPaused() {
			t.Error("state 10: IsPaused=false, want true")
		}
		for s := VacuumStateID(1); s <= 20; s++ {
			if s == 10 {
				continue
			}
			if s.IsPaused() {
				t.Errorf("state %d: IsPaused=true, want false", s)
			}
		}
	})

	t.Run("IsError", func(t *testing.T) {
		errStates := []VacuumStateID{9, 12}
		for _, s := range errStates {
			if !s.IsError() {
				t.Errorf("state %d: IsError=false, want true", s)
			}
		}
		for s := VacuumStateID(1); s <= 20; s++ {
			isExpected := s == 9 || s == 12
			if !isExpected && s.IsError() {
				t.Errorf("state %d: IsError=true, want false", s)
			}
		}
	})

	t.Run("IsInTransit", func(t *testing.T) {
		for _, s := range []VacuumStateID{6, 15} {
			if !s.IsInTransit() {
				t.Errorf("state %d: IsInTransit=false, want true", s)
			}
		}
		for s := VacuumStateID(1); s <= 20; s++ {
			isExpected := s == 6 || s == 15
			if !isExpected && s.IsInTransit() {
				t.Errorf("state %d: IsInTransit=true, want false", s)
			}
		}
	})

	t.Run("IsIdle", func(t *testing.T) {
		idle := []VacuumStateID{3, 2, 8, 100}
		for _, s := range idle {
			if !s.IsIdle() {
				t.Errorf("state %d: IsIdle=false, want true", s)
			}
		}
		notIdle := []VacuumStateID{1, 4, 5, 7, 12, 13, 14}
		for _, s := range notIdle {
			if s.IsIdle() {
				t.Errorf("state %d: IsIdle=true, want false", s)
			}
		}
	})
}

func TestUnmarshalCleanSummary(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		raw := json.RawMessage(`[100, 200, 5, [1700000000, 1699913600]]`)
		cs, err := UnmarshalCleanSummary(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cs.TotalTime != 100 || cs.TotalArea != 200 || cs.TotalCount != 5 {
			t.Errorf("got %+v", cs)
		}
		if len(cs.Records) != 2 || cs.Records[0] != 1700000000 || cs.Records[1] != 1699913600 {
			t.Errorf("records = %v", cs.Records)
		}
	})

	t.Run("empty records", func(t *testing.T) {
		raw := json.RawMessage(`[100, 200, 5, []]`)
		cs, err := UnmarshalCleanSummary(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(cs.Records) != 0 {
			t.Errorf("Records len=%d, want 0", len(cs.Records))
		}
		if !cs.LastCleanTime().IsZero() {
			t.Errorf("LastCleanTime = %v, want zero", cs.LastCleanTime())
		}
	})

	t.Run("wrong element count", func(t *testing.T) {
		raw := json.RawMessage(`[100, 200, 5]`)
		_, err := UnmarshalCleanSummary(raw)
		if err == nil {
			t.Error("expected error for 3 elements")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		raw := json.RawMessage(`not json`)
		_, err := UnmarshalCleanSummary(raw)
		if err == nil {
			t.Error("expected error for invalid json")
		}
	})

	t.Run("null records", func(t *testing.T) {
		raw := json.RawMessage(`[100, 200, 5, null]`)
		cs, err := UnmarshalCleanSummary(raw)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if cs.Records != nil {
			t.Errorf("Records = %v, want nil", cs.Records)
		}
		// Should not panic.
		if !cs.LastCleanTime().IsZero() {
			t.Errorf("LastCleanTime = %v, want zero", cs.LastCleanTime())
		}
	})
}

func TestLastCleanTime(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		cs := CleanSummary{}
		if !cs.LastCleanTime().IsZero() {
			t.Errorf("got %v, want zero", cs.LastCleanTime())
		}
	})

	t.Run("single record", func(t *testing.T) {
		cs := CleanSummary{Records: []int64{1700000000}}
		want := time.Unix(1700000000, 0)
		if !cs.LastCleanTime().Equal(want) {
			t.Errorf("got %v, want %v", cs.LastCleanTime(), want)
		}
	})

	t.Run("returns first (newest)", func(t *testing.T) {
		cs := CleanSummary{Records: []int64{1700000000, 1699000000}}
		want := time.Unix(1700000000, 0)
		if !cs.LastCleanTime().Equal(want) {
			t.Errorf("got %v, want %v", cs.LastCleanTime(), want)
		}
	})
}
