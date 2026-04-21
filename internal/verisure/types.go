package verisure

// ArmState represents the Verisure alarm state.
type ArmState string

const (
	ArmStateArmedAway ArmState = "ARMED_AWAY"
	ArmStateArmedHome ArmState = "ARMED_HOME"
	ArmStateDisarmed  ArmState = "DISARMED"
	ArmStateUnknown   ArmState = ""
)

// IsArmedAway reports whether the alarm is in the away-armed state.
func (a ArmState) IsArmedAway() bool { return a == ArmStateArmedAway }

// IsDisengaged reports whether the alarm is disarmed or home-armed
// (either way, someone is home and the vacuum should stop).
func (a ArmState) IsDisengaged() bool {
	return a == ArmStateDisarmed || a == ArmStateArmedHome
}

// mfaValidateRequest submits the SMS code to /auth/mfa/validate.
type mfaValidateRequest struct {
	Code string `json:"code"`
}

// installationsResponse is the response from GET /installation.
type installationsResponse []struct {
	GIID  string `json:"giid"`
	Alias string `json:"alias"`
}

// armStateResponse is the response from GET /installation/{giid}/armstate.
type armStateResponse struct {
	Data struct {
		State      ArmState `json:"state"`
		StatusType string   `json:"statusType"`
		Date       string   `json:"date"`
		Name       string   `json:"name"`
		ChangedVia string   `json:"changedVia"`
	} `json:"data"`
}
