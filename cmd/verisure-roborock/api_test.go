package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/roborock"
	"verisure-roborock/internal/store"
)

type fakeAPIVacuum struct {
	name         string
	host         string
	status       roborock.VacuumStatus
	statusCalls  int
	startCalls   int
	startPaused  bool
	pauseCalls   int
	chargeCalls  int
	roomCalls    int
	rooms        []int
	repeat       int
	cleanSummary roborock.CleanSummary
}

func (f *fakeAPIVacuum) Name() string { return f.name }
func (f *fakeAPIVacuum) Host() string { return f.host }
func (f *fakeAPIVacuum) Status(context.Context) (roborock.VacuumStatus, error) {
	f.statusCalls++
	return f.status, nil
}
func (f *fakeAPIVacuum) CleanSummary(context.Context) (roborock.CleanSummary, error) {
	return f.cleanSummary, nil
}
func (f *fakeAPIVacuum) StartOrResume(_ context.Context, paused bool) error {
	f.startCalls++
	f.startPaused = paused
	return nil
}
func (f *fakeAPIVacuum) Pause(context.Context) error {
	f.pauseCalls++
	return nil
}
func (f *fakeAPIVacuum) Charge(context.Context) error {
	f.chargeCalls++
	return nil
}
func (f *fakeAPIVacuum) CleanRooms(_ context.Context, rooms []int, repeat int) error {
	f.roomCalls++
	f.rooms = append([]int(nil), rooms...)
	f.repeat = repeat
	return nil
}

func newAPITestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRobotAPIListStartStopAndRooms(t *testing.T) {
	upstairs := &fakeAPIVacuum{
		name:   "upstairs",
		host:   "192.0.2.10",
		status: roborock.VacuumStatus{State: roborock.StateIdle, Battery: 90},
	}
	downstairs := &fakeAPIVacuum{
		name:   "downstairs",
		host:   "192.0.2.11",
		status: roborock.VacuumStatus{State: roborock.StatePaused, Battery: 80},
	}
	st := newAPITestStore(t)
	if err := st.SetVacuumStartedByUs(upstairs.host, true); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRobotHandlers(mux,
		[]config.VacuumConfig{
			{Name: upstairs.name, Host: upstairs.host, Backend: "roborock"},
			{Name: downstairs.name, Host: downstairs.host, Backend: "roborock"},
		},
		[]robotVacuum{upstairs, downstairs},
		st,
	)

	t.Run("list robots", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/robots", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var got struct {
			Robots []struct {
				Name        string `json:"name"`
				Host        string `json:"host"`
				Backend     string `json:"backend"`
				StartedByUs bool   `json:"started_by_us"`
			} `json:"robots"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Robots) != 2 {
			t.Fatalf("robots = %d, want 2", len(got.Robots))
		}
		if got.Robots[0].Name != "upstairs" || !got.Robots[0].StartedByUs {
			t.Fatalf("first robot = %+v", got.Robots[0])
		}
		if got.Robots[1].Backend != "roborock" {
			t.Fatalf("second backend = %q", got.Robots[1].Backend)
		}
	})

	t.Run("start robot", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/robots/downstairs/start", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		if downstairs.statusCalls != 1 || downstairs.startCalls != 1 || !downstairs.startPaused {
			t.Fatalf("status/start = %d/%d paused=%v", downstairs.statusCalls, downstairs.startCalls, downstairs.startPaused)
		}
		if !st.Get().Vacuums[downstairs.host].StartedByUs {
			t.Fatal("StartedByUs was not persisted")
		}
	})

	t.Run("start selected rooms", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/robots/upstairs/start", strings.NewReader(`{"rooms":[16,17],"repeat":2}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		if upstairs.roomCalls != 1 || upstairs.startCalls != 0 {
			t.Fatalf("room/start calls = %d/%d", upstairs.roomCalls, upstairs.startCalls)
		}
		if upstairs.repeat != 2 || len(upstairs.rooms) != 2 || upstairs.rooms[0] != 16 || upstairs.rooms[1] != 17 {
			t.Fatalf("rooms=%v repeat=%d", upstairs.rooms, upstairs.repeat)
		}
	})

	t.Run("stop robot docks", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/robots/upstairs/stop", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		if upstairs.pauseCalls != 1 || upstairs.chargeCalls != 1 {
			t.Fatalf("pause/charge = %d/%d", upstairs.pauseCalls, upstairs.chargeCalls)
		}
		if st.Get().Vacuums[upstairs.host].StartedByUs {
			t.Fatal("StartedByUs was not cleared")
		}
	})
}
