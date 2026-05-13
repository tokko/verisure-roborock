package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"verisure-roborock/internal/config"
	"verisure-roborock/internal/controller"
	"verisure-roborock/internal/store"
)

type robotVacuum = controller.VacuumCommander

type roomCleaner interface {
	CleanRooms(ctx context.Context, rooms []int, repeat int) error
}

type robotInfo struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Backend     string `json:"backend"`
	StartedByUs bool   `json:"started_by_us"`
}

type startRobotRequest struct {
	Rooms  []int `json:"rooms"`
	Repeat int   `json:"repeat"`
}

func registerRobotHandlers(mux *http.ServeMux, cfg []config.VacuumConfig, vacuums []robotVacuum, st *store.Store) {
	mux.HandleFunc("GET /robots", func(w http.ResponseWriter, r *http.Request) {
		robots := make([]robotInfo, 0, len(vacuums))
		state := st.Get()
		for _, v := range vacuums {
			robots = append(robots, robotInfo{
				Name:        v.Name(),
				Host:        v.Host(),
				Backend:     backendForVacuum(cfg, v),
				StartedByUs: state.Vacuums[v.Host()].StartedByUs,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"robots": robots})
	})

	mux.HandleFunc("POST /robots/{robot}/start", func(w http.ResponseWriter, r *http.Request) {
		v, ok := findVacuum(vacuums, r.PathValue("robot"))
		if !ok {
			writeError(w, http.StatusNotFound, "robot not found")
			return
		}
		var req startRobotRequest
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(req.Rooms) > 0 {
			if err := validateRoomRequest(req); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			cleaner, ok := v.(roomCleaner)
			if !ok {
				writeError(w, http.StatusNotImplemented, "room cleaning is not supported for this robot backend")
				return
			}
			if err := cleaner.CleanRooms(r.Context(), req.Rooms, req.Repeat); err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			if err := st.SetVacuumStartedByUs(v.Host(), true); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "robot": v.Name(), "action": "clean_rooms"})
			return
		}

		status, err := v.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("get status: %v", err))
			return
		}
		if err := v.StartOrResume(r.Context(), status.State.IsPaused()); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if err := st.SetVacuumStartedByUs(v.Host(), true); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "robot": v.Name(), "action": "start"})
	})

	mux.HandleFunc("POST /robots/{robot}/stop", func(w http.ResponseWriter, r *http.Request) {
		v, ok := findVacuum(vacuums, r.PathValue("robot"))
		if !ok {
			writeError(w, http.StatusNotFound, "robot not found")
			return
		}
		if err := v.Pause(r.Context()); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("pause: %v", err))
			return
		}
		if err := v.Charge(r.Context()); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("charge: %v", err))
			return
		}
		if err := st.SetVacuumStartedByUs(v.Host(), false); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "robot": v.Name(), "action": "stop"})
	})
}

func backendForVacuum(cfg []config.VacuumConfig, v robotVacuum) string {
	for _, vc := range cfg {
		if vc.Host == v.Host() || strings.EqualFold(vc.Name, v.Name()) {
			return vc.Backend
		}
	}
	return ""
}

func findVacuum(vacuums []robotVacuum, selector string) (robotVacuum, bool) {
	selector = strings.TrimSpace(selector)
	for _, v := range vacuums {
		if strings.EqualFold(v.Name(), selector) || strings.EqualFold(v.Host(), selector) {
			return v, true
		}
	}
	return nil, false
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func validateRoomRequest(req startRobotRequest) error {
	if req.Repeat < 0 {
		return errors.New("repeat must be positive")
	}
	for _, room := range req.Rooms {
		if room <= 0 {
			return errors.New("rooms must contain positive room ids")
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
