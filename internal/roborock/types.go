package roborock

import (
	"encoding/json"
	"fmt"
	"time"
)

// VacuumStateID is the numeric state from get_status.
type VacuumStateID int

const (
	StateInitiating   VacuumStateID = 1
	StateSleep        VacuumStateID = 2
	StateIdle         VacuumStateID = 3
	StateRemoteCtrl   VacuumStateID = 4
	StateCleaning     VacuumStateID = 5
	StateReturning    VacuumStateID = 6
	StateManual       VacuumStateID = 7
	StateCharging     VacuumStateID = 8
	StateChargingErr  VacuumStateID = 9
	StatePaused       VacuumStateID = 10
	StateSpotClean    VacuumStateID = 11
	StateError        VacuumStateID = 12
	StateShuttingDown VacuumStateID = 13
	StateUpdating     VacuumStateID = 14
	StateDocking      VacuumStateID = 15
	StateZoneCleaning VacuumStateID = 17
	StateRoomCleaning VacuumStateID = 18
	StateFullyCharged VacuumStateID = 100
)

// IsActiveClean reports whether the vacuum is running a cleaning cycle.
func (s VacuumStateID) IsActiveClean() bool {
	return s == StateCleaning || s == StateSpotClean ||
		s == StateZoneCleaning || s == StateRoomCleaning
}

// IsPaused reports whether the vacuum is paused mid-clean.
func (s VacuumStateID) IsPaused() bool { return s == StatePaused }

// IsError reports whether the vacuum is in an error condition.
func (s VacuumStateID) IsError() bool { return s == StateError || s == StateChargingErr }

// IsInTransit reports whether the vacuum is returning to dock or docking.
func (s VacuumStateID) IsInTransit() bool { return s == StateReturning || s == StateDocking }

// IsIdle reports whether the vacuum is idle, sleeping, charging, or fully charged
// (i.e. not doing anything and safe to start).
func (s VacuumStateID) IsIdle() bool {
	return s == StateIdle || s == StateSleep ||
		s == StateCharging || s == StateFullyCharged
}

// String returns a human-readable state name.
func (s VacuumStateID) String() string {
	names := map[VacuumStateID]string{
		StateInitiating:   "initiating",
		StateSleep:        "sleep",
		StateIdle:         "idle",
		StateRemoteCtrl:   "remote-control",
		StateCleaning:     "cleaning",
		StateReturning:    "returning",
		StateManual:       "manual",
		StateCharging:     "charging",
		StateChargingErr:  "charging-error",
		StatePaused:       "paused",
		StateSpotClean:    "spot-cleaning",
		StateError:        "error",
		StateShuttingDown: "shutting-down",
		StateUpdating:     "updating",
		StateDocking:      "docking",
		StateZoneCleaning: "zone-cleaning",
		StateRoomCleaning: "room-cleaning",
		StateFullyCharged: "fully-charged",
	}
	if n, ok := names[s]; ok {
		return n
	}
	return fmt.Sprintf("unknown(%d)", int(s))
}

// VacuumStatus is the response from get_status.
type VacuumStatus struct {
	State      VacuumStateID `json:"state"`
	InCleaning int           `json:"in_cleaning"`
	CleanTime  int           `json:"clean_time"` // seconds
	CleanArea  int           `json:"clean_area"` // cm²
	Battery    int           `json:"battery"`    // 0-100
	ErrorCode  int           `json:"error_code"`
}

// CleanSummary holds the parsed result of get_clean_summary.
//
// The raw response is a heterogeneous JSON array:
//
//	[total_time_int, total_area_int, total_count_int, [ts1, ts2, ...]]
//
// It cannot be decoded directly into a struct.
type CleanSummary struct {
	TotalTime  int
	TotalArea  int
	TotalCount int
	Records    []int64 // Unix timestamps of completed full cleans, newest first
}

// UnmarshalCleanSummary parses the raw JSON result of get_clean_summary.
func UnmarshalCleanSummary(raw json.RawMessage) (CleanSummary, error) {
	// Decode as a 4-element generic array.
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return CleanSummary{}, fmt.Errorf("clean_summary: %w", err)
	}
	if len(parts) < 4 {
		return CleanSummary{}, fmt.Errorf("clean_summary: expected 4 elements, got %d", len(parts))
	}

	var cs CleanSummary
	if err := json.Unmarshal(parts[0], &cs.TotalTime); err != nil {
		return CleanSummary{}, fmt.Errorf("clean_summary total_time: %w", err)
	}
	if err := json.Unmarshal(parts[1], &cs.TotalArea); err != nil {
		return CleanSummary{}, fmt.Errorf("clean_summary total_area: %w", err)
	}
	if err := json.Unmarshal(parts[2], &cs.TotalCount); err != nil {
		return CleanSummary{}, fmt.Errorf("clean_summary total_count: %w", err)
	}
	if err := json.Unmarshal(parts[3], &cs.Records); err != nil {
		return CleanSummary{}, fmt.Errorf("clean_summary records: %w", err)
	}
	return cs, nil
}

// LastCleanTime returns the most recent completed clean timestamp.
// Returns zero time if Records is empty (never cleaned).
// The controller treats zero time as "needs cleaning".
func (s CleanSummary) LastCleanTime() time.Time {
	if len(s.Records) == 0 {
		return time.Time{}
	}
	return time.Unix(s.Records[0], 0)
}
