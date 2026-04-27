package verisure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
)

const (
	applicationID    = "PS_PYTHON"
	defaultLoginBase = "https://automation01.verisure.com"
	fallbackLoginBase = "https://automation02.verisure.com"
)

// mfaState tracks a pending MFA flow so we never send a second SMS
// while still waiting for the user to submit the first code.
type mfaState struct {
	loginBase    string
	stepupCookie string
}

// Client is a Verisure REST API client with session cookie management.
type Client struct {
	apiBase  string
	email    string
	password string
	phone    string

	http *http.Client
	jar  *cookiejar.Jar
	mu   sync.Mutex

	authed bool
	giid   string

	// pendingMFA is non-nil when an SMS has been sent and we're waiting
	// for the user to submit the code. While set, loginLocked goes straight
	// to the validate step without sending a new SMS.
	pendingMFA *mfaState

	// mfaCh receives the SMS code from the HTTP handler.
	mfaCh chan string
}

func NewClient(apiBase, email, password, phone, persistedCookie string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	c := &Client{
		apiBase:  apiBase,
		email:    email,
		password: password,
		phone:    phone,
		jar:      jar,
		mfaCh:    make(chan string, 1),
	}
	c.http = &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if persistedCookie != "" {
		// persistedCookie is stored as "cookieName=cookieValue" by SessionCookie().
		// Split on the first "=" so we restore the cookie under the right name
		// (e.g. "vs-access" or "vid"), not always under "vid".
		if eq := strings.Index(persistedCookie, "="); eq > 0 {
			name := persistedCookie[:eq]
			value := persistedCookie[eq+1:]
			c.setCookie(apiBase, name, value)
			c.authed = true
			slog.Debug("verisure: restored session from store", "cookie_name", name)
		} else {
			slog.Warn("verisure: ignoring malformed persisted cookie")
		}
	}
	return c, nil
}

func (c *Client) SubmitMFACode(code string) {
	select {
	case c.mfaCh <- code:
	default:
	}
}

func (c *Client) SessionCookie() string {
	// The API now returns vs-access (JWT) instead of vid. Persist whichever
	// session cookie we have so we can resume without re-authenticating.
	for _, base := range []string{c.apiBase, "https://www.verisure.com"} {
		u, _ := url.Parse(base)
		for _, cookie := range c.jar.Cookies(u) {
			if cookie.Name == "vs-access" || cookie.Name == "vid" {
				return cookie.Name + "=" + cookie.Value
			}
		}
	}
	return ""
}

func (c *Client) ArmState(ctx context.Context) (ArmState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Login (including MFA) if not yet authenticated.
	if !c.authed {
		if err := c.loginLocked(ctx); err != nil {
			return ArmStateUnknown, fmt.Errorf("verisure login: %w", err)
		}
	}

	// GIID discovery is separate: a transient 503 here must not clear the
	// MFA session and force another SMS.
	if c.giid == "" {
		if err := c.discoverGIIDLocked(ctx); err != nil {
			return ArmStateUnknown, fmt.Errorf("verisure installation discovery: %w", err)
		}
	}

	state, err := c.fetchArmStateLocked(ctx)
	if err != nil {
		// Only redo the full login on a genuine session-expiry error.
		if isSessionExpired(err) {
			c.authed = false
			c.giid = ""
			if loginErr := c.loginLocked(ctx); loginErr != nil {
				return ArmStateUnknown, fmt.Errorf("verisure re-login: %w", loginErr)
			}
			if c.giid == "" {
				if discErr := c.discoverGIIDLocked(ctx); discErr != nil {
					return ArmStateUnknown, fmt.Errorf("verisure re-discovery: %w", discErr)
				}
			}
			state, err = c.fetchArmStateLocked(ctx)
		}
	}
	return state, err
}

// isSessionExpired returns true for HTTP 401/403 errors that indicate the
// session cookie has expired — as opposed to transient server errors (503 etc.)
// which should be retried without a full re-login.
func isSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 401") || strings.Contains(s, "HTTP 403") ||
		strings.Contains(s, "session expired")
}

func (c *Client) fetchArmStateLocked(ctx context.Context) (ArmState, error) {
	var resp armStateResponse
	if err := c.get(ctx, fmt.Sprintf("/installation/%s/armstate", c.giid), &resp); err != nil {
		return ArmStateUnknown, err
	}
	return resp.Data.State, nil
}

// loginLocked authenticates. If an MFA flow is already in progress
// (pendingMFA != nil), it goes straight to validate without sending a new SMS.
func (c *Client) loginLocked(ctx context.Context) error {
	slog.Info("verisure: logging in", "email", c.email)

	// If we're already mid-MFA (SMS was sent, waiting for code) skip
	// the /auth/login POST and go straight to validate. This prevents
	// sending a new SMS on every retry.
	if c.pendingMFA != nil {
		slog.Info("verisure: resuming pending MFA flow, waiting for SMS code")
		if err := c.validateMFALocked(ctx, c.pendingMFA); err != nil {
			// Bad/expired code — clear state so next attempt restarts the flow.
			c.pendingMFA = nil
			return fmt.Errorf("MFA validate: %w", err)
		}
		c.pendingMFA = nil
		return c.finishLoginLocked(ctx)
	}

	// Normal flow: POST /auth/login, try each base until one accepts.
	var pending *mfaState
	var lastErr error

	for _, base := range []string{defaultLoginBase, fallbackLoginBase} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/login", http.NoBody)
		if err != nil {
			lastErr = err
			continue
		}
		req.SetBasicAuth(c.email, c.password)
		req.Header.Set("APPLICATION_ID", applicationID)
		req.Header.Set("Accept", "application/json")
		req.ContentLength = 0

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("POST %s/auth/login: %w", base, err)
			slog.Debug("verisure: login base unreachable", "base", base, "err", lastErr)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("POST /auth/login: HTTP %d: %s", resp.StatusCode, body)
			slog.Debug("verisure: login base rejected", "base", base, "status", resp.StatusCode)
			continue
		}

		c.propagateCookies(base, resp.Cookies())

		if strings.Contains(string(body), "stepUpToken") {
			// MFA required — extract the stepup cookie.
			var stepup string
			for _, cookie := range resp.Cookies() {
				if cookie.Name == "vs-stepup" {
					stepup = cookie.Value
				}
			}
			if stepup == "" {
				lastErr = fmt.Errorf("MFA required but no vs-stepup cookie from %s", base)
				continue
			}
			pending = &mfaState{loginBase: base, stepupCookie: stepup}
		} else {
			// No MFA: vid cookie is already in the jar.
			slog.Info("verisure: login succeeded (no MFA)")
		}
		lastErr = nil
		break
	}

	if lastErr != nil {
		return lastErr
	}

	// MFA path: send SMS once, store state, wait for code.
	if pending != nil {
		if err := c.sendSMSLocked(ctx, pending); err != nil {
			return fmt.Errorf("trigger SMS: %w", err)
		}
		// Store pending state before blocking — if the context times out,
		// the next call to loginLocked will resume the validate step.
		c.pendingMFA = pending

		if err := c.validateMFALocked(ctx, pending); err != nil {
			// Code wrong or expired; pendingMFA stays set so next retry
			// asks for a new code (without sending another SMS).
			// But if the error suggests the session is dead, clear it.
			if strings.Contains(err.Error(), "HTTP 4") {
				c.pendingMFA = nil
			}
			return fmt.Errorf("MFA validate: %w", err)
		}
		c.pendingMFA = nil
	}

	return c.finishLoginLocked(ctx)
}

// sendSMSLocked POSTs to /auth/mfa to trigger the SMS code.
func (c *Client) sendSMSLocked(ctx context.Context, state *mfaState) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		state.loginBase+"/auth/mfa", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "vs-stepup", Value: state.stepupCookie})
	req.ContentLength = 0

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST /auth/mfa: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /auth/mfa: HTTP %d", resp.StatusCode)
	}

	slog.Warn("verisure: SMS code sent — POST the code to /mfa-code to continue",
		"phone", c.phone)
	return nil
}

// validateMFALocked waits for a code on mfaCh then POSTs to /auth/mfa/validate.
func (c *Client) validateMFALocked(ctx context.Context, state *mfaState) error {
	// Drain any stale code from a previous failed attempt.
	select {
	case <-c.mfaCh:
	default:
	}

	// Block until the operator submits a code.
	var code string
	select {
	case code = <-c.mfaCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	payload, _ := json.Marshal(mfaValidateRequest{Token: code, Trust: false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		state.loginBase+"/auth/mfa/validate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "vs-stepup", Value: state.stepupCookie})

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST /auth/mfa/validate: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /auth/mfa/validate: HTTP %d: %s", resp.StatusCode, body)
	}

	c.propagateCookies(state.loginBase, resp.Cookies())
	slog.Info("verisure: MFA code accepted")
	return nil
}

// finishLoginLocked marks the session as authenticated.
// GIID discovery is intentionally deferred to ArmState so that a transient
// 503 on the installation endpoint does not force a full re-login (new SMS).
func (c *Client) finishLoginLocked(ctx context.Context) error {
	c.authed = true
	slog.Info("verisure: session authenticated (GIID discovery deferred)")
	return nil
}

func (c *Client) discoverGIIDLocked(ctx context.Context) error {
	if c.giid != "" {
		return nil
	}
	path := "/installation/search?email=" + url.QueryEscape(c.email)
	var installations installationsResponse
	if err := c.get(ctx, path, &installations); err != nil {
		return err
	}
	if len(installations) == 0 {
		return fmt.Errorf("no installations found")
	}
	c.giid = installations[0].GIID
	slog.Debug("verisure: discovered installation", "giid", c.giid, "alias", installations[0].Alias)
	return nil
}

func (c *Client) SetGIID(giid string) {
	c.mu.Lock()
	c.giid = giid
	c.mu.Unlock()
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HTTP %d (session expired)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) setCookie(base, name, value string) {
	u, _ := url.Parse(base)
	c.jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value}})
}

func (c *Client) propagateCookies(loginBase string, cookies []*http.Cookie) {
	for _, base := range []string{loginBase, c.apiBase, "https://www.verisure.com"} {
		u, _ := url.Parse(base)
		c.jar.SetCookies(u, cookies)
	}
}
