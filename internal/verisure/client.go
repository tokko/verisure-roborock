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
	"sync"
)

// Client is a Verisure REST API client with session cookie management.
// Authentication is session-cookie based (the "vid" cookie).
// If 2FA is configured, the client will trigger an SMS and block until
// a code is submitted via SubmitMFACode.
type Client struct {
	baseURL  string
	email    string
	password string
	phone    string // MFA phone number

	http   *http.Client
	jar    *cookiejar.Jar
	mu     sync.Mutex
	authed bool
	giid   string // installation ID, discovered on first login

	// mfaCh receives the SMS code from the HTTP handler (see SubmitMFACode).
	mfaCh chan string
}

// NewClient creates a Verisure client. Provide an optional persisted cookie
// value (from store.State.VerisureCookie) to resume a previous session.
func NewClient(baseURL, email, password, phone, persistedCookie string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	c := &Client{
		baseURL:  baseURL,
		email:    email,
		password: password,
		phone:    phone,
		jar:      jar,
		mfaCh:    make(chan string, 1),
	}
	c.http = &http.Client{
		Jar: jar,
		// Do not follow redirects automatically — we check them manually.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Restore persisted session cookie if provided.
	if persistedCookie != "" {
		u, _ := url.Parse(baseURL)
		jar.SetCookies(u, []*http.Cookie{
			{Name: "vid", Value: persistedCookie},
		})
		c.authed = true
		slog.Debug("verisure: restored session from store")
	}

	return c, nil
}

// SubmitMFACode is called by the HTTP handler when the operator POSTs
// the SMS code to /mfa-code. It unblocks the pending login.
func (c *Client) SubmitMFACode(code string) {
	select {
	case c.mfaCh <- code:
	default:
		// Already have a code queued (shouldn't happen in normal use).
	}
}

// SessionCookie returns the current "vid" cookie value for persistence.
func (c *Client) SessionCookie() string {
	u, _ := url.Parse(c.baseURL)
	for _, cookie := range c.jar.Cookies(u) {
		if cookie.Name == "vid" {
			return cookie.Value
		}
	}
	return ""
}

// ArmState fetches the current alarm state.
func (c *Client) ArmState(ctx context.Context) (ArmState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.authed {
		if err := c.loginLocked(ctx); err != nil {
			return ArmStateUnknown, fmt.Errorf("verisure login: %w", err)
		}
	}

	state, err := c.fetchArmStateLocked(ctx)
	if err != nil {
		// On auth error, invalidate session and try once more.
		c.authed = false
		if loginErr := c.loginLocked(ctx); loginErr != nil {
			return ArmStateUnknown, fmt.Errorf("verisure re-login: %w", loginErr)
		}
		state, err = c.fetchArmStateLocked(ctx)
	}
	return state, err
}

func (c *Client) fetchArmStateLocked(ctx context.Context) (ArmState, error) {
	var resp armStateResponse
	if err := c.get(ctx, fmt.Sprintf("/installation/%s/armstate", c.giid), &resp); err != nil {
		return ArmStateUnknown, err
	}
	return resp.Data.State, nil
}

// loginLocked performs the full login flow (must be called with c.mu held).
func (c *Client) loginLocked(ctx context.Context) error {
	slog.Info("verisure: logging in", "email", c.email)

	// Step 1: POST /cookie with credentials.
	if err := c.postJSON(ctx, "/cookie", loginRequest{
		Login:    c.email,
		Password: c.password,
	}, nil); err != nil {
		return fmt.Errorf("POST /cookie: %w", err)
	}

	// Step 2: Check if MFA is required by probing /installation.
	// If we get 401/403, MFA step is needed.
	if err := c.discoverGIIDLocked(ctx); err != nil {
		// Try MFA flow if we haven't got a GIID yet.
		slog.Info("verisure: MFA required, requesting SMS code")
		if mfaErr := c.mfaFlowLocked(ctx); mfaErr != nil {
			return fmt.Errorf("MFA flow: %w", mfaErr)
		}
		// Retry GIID discovery after MFA.
		if err := c.discoverGIIDLocked(ctx); err != nil {
			return fmt.Errorf("GIID discovery after MFA: %w", err)
		}
	}

	c.authed = true
	slog.Info("verisure: authenticated", "giid", c.giid)
	return nil
}

// mfaFlowLocked triggers SMS and blocks until a code arrives via SubmitMFACode.
func (c *Client) mfaFlowLocked(ctx context.Context) error {
	// Drain any stale code.
	select {
	case <-c.mfaCh:
	default:
	}

	// POST /mfa to trigger SMS to registered phone.
	if err := c.postJSON(ctx, "/mfa", mfaRequest{CallMe: false}, nil); err != nil {
		return fmt.Errorf("trigger SMS: %w", err)
	}

	slog.Warn("verisure: SMS code sent — POST the code to /mfa-code to continue")

	// Wait for operator to submit the code (via HTTP endpoint).
	var code string
	select {
	case code = <-c.mfaCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Submit the code.
	if err := c.postJSON(ctx, "/mfa/code", mfaCodeRequest{Token: code}, nil); err != nil {
		return fmt.Errorf("submit MFA code: %w", err)
	}

	slog.Info("verisure: MFA code accepted")
	return nil
}

// discoverGIIDLocked fetches the installation list and sets c.giid if not already set.
func (c *Client) discoverGIIDLocked(ctx context.Context) error {
	if c.giid != "" {
		return nil
	}
	var installations installationsResponse
	if err := c.get(ctx, "/installation", &installations); err != nil {
		return err
	}
	if len(installations) == 0 {
		return fmt.Errorf("no installations found")
	}
	c.giid = installations[0].GIID
	slog.Debug("verisure: discovered installation", "giid", c.giid, "alias", installations[0].Alias)
	return nil
}

// SetGIID pre-configures the installation ID (skips auto-discovery).
func (c *Client) SetGIID(giid string) {
	c.mu.Lock()
	c.giid = giid
	c.mu.Unlock()
}

// get performs an authenticated GET and decodes the JSON response.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
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

// postJSON performs a POST with a JSON body, optionally decoding the response.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
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
