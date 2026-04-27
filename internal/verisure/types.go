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
// Field name is "token" (not "code") — confirmed by direct API testing.
type mfaValidateRequest struct {
	Token string `json:"token"`
	Trust bool   `json:"trust"` // false = don't persist trust on this device
}

// GraphQL response types for the automation01.verisure.com API.

type graphQLInstallationsResponse struct {
	Data struct {
		Account struct {
			Installations []struct {
				GIID  string `json:"giid"`
				Alias string `json:"alias"`
			} `json:"installations"`
		} `json:"account"`
	} `json:"data"`
}

type graphQLArmStateResponse struct {
	Data struct {
		Installation struct {
			ArmState struct {
				StatusType string `json:"statusType"`
				Date       string `json:"date"`
				Name       string `json:"name"`
				ChangedVia string `json:"changedVia"`
			} `json:"armState"`
		} `json:"installation"`
	} `json:"data"`
}
