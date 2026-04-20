package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// VacuumEntry holds per-vacuum persistent state.
type VacuumEntry struct {
	Name          string    `json:"name"`
	LastCleanTime time.Time `json:"last_clean_time"`
	StartedByUs   bool      `json:"started_by_us"`
}

// State is the full persisted state, written atomically to disk.
type State struct {
	AlarmState   string                 `json:"alarm_state"`
	ControlState string                 `json:"control_state"`
	Vacuums      map[string]VacuumEntry `json:"vacuums"` // key: host IP
	// Verisure session cookie value — persisted to avoid repeated MFA prompts.
	VerisureCookie string `json:"verisure_cookie,omitempty"`
}

// Store is a thread-safe, atomically-persisted state file.
type Store struct {
	path string
	mu   sync.Mutex
	s    State
}

// New opens an existing state file or creates a fresh one.
func New(path string) (*Store, error) {
	st := &Store{
		path: path,
		s:    State{Vacuums: make(map[string]VacuumEntry)},
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &st.s); err != nil {
		return nil, err
	}
	if st.s.Vacuums == nil {
		st.s.Vacuums = make(map[string]VacuumEntry)
	}
	return st, nil
}

// Get returns a deep copy of the current state.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := State{
		AlarmState:     s.s.AlarmState,
		ControlState:   s.s.ControlState,
		VerisureCookie: s.s.VerisureCookie,
		Vacuums:        make(map[string]VacuumEntry, len(s.s.Vacuums)),
	}
	for k, v := range s.s.Vacuums {
		cp.Vacuums[k] = v
	}
	return cp
}

func (s *Store) SetAlarmState(a string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.AlarmState = a
	return s.save()
}

func (s *Store) SetControlState(cs string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.ControlState = cs
	return s.save()
}

func (s *Store) SetVacuumStartedByUs(host string, v bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.s.Vacuums[host]
	e.StartedByUs = v
	s.s.Vacuums[host] = e
	return s.save()
}

func (s *Store) SetVacuumLastClean(host string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.s.Vacuums[host]
	e.LastCleanTime = t
	s.s.Vacuums[host] = e
	return s.save()
}

func (s *Store) SetVacuumName(host, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.s.Vacuums[host]
	e.Name = name
	s.s.Vacuums[host] = e
	return s.save()
}

func (s *Store) SetVerisureCookie(cookie string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.s.VerisureCookie = cookie
	return s.save()
}

// save writes state atomically: temp file then rename.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
