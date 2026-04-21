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
	// applicationID is the header value the Verisure app API expects.
	applicationID = "PS_PYTHON"

	// defaultLoginBase is the primary authentication server.
	defaultLoginBase = "https://automation01.verisure.com"

	// fallbackLoginBase is tried if the primary returns an error.
	fallbackLoginBase = "https://automation02.verisure.com"
)

// Client is a Verisure REST API client with session cookie management.
// Authentication uses HTTP Basic Auth against automation01.verisure.com.
// If 2FA is configured the client triggers an SMS and blocks until a code
// is submitted via SubmitMFACode.
type Client struct {
	apiBase  string // e-api01.verisure.com/xbn/2 — data calls
	email    string
	password string
	phone    string // MFA phone number (informational only — SMS is automatic)

	http   *http.Client
	jar    *cookiejar.Jar
	mu     sync.Mutex
	authed bool
	giid   string // installation ID, discovered on first login

	// stepupCookie holds the vs-stepup cookie value across the MFA flow.
	stepupCookie string

	// mfaCh receives the SMS code from the HTTP handler (see SubmitMFACode).
	mfaCh chan string
}

// NewClient creates a Verisure client.
// apiBase is the data API base URL (e.g. "https://e-api01.verisure.com/xbn/2").
// Provide persistedCookie to resume a previous session without re-authenticating.
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
		c.setCookie(apiBase, "vid", persistedCookie)
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
	}
}

// SessionCookie returns the current "vid" cookie value for persistence.
func (c *Client) SessionCookie() string {
	u, _ := url.Parse(c.apiBase)
	for _, cookie := range c.jar.Cookies(u) {
		if cookie.Name == "vid" {
			return cookie.Value
		}
	}
	// Also check the verisure.com domain.
	u2, _ := url.Parse("https://www.verisure.com")
	for _, cookie := range c.jar.Cookies(u2) {
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
		// On auth error, invalidate session and retry once.
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

// loginLocked performs the full login flow. Must be called with c.mu held.
// Tries automation01 first; falls back to automation02 only if the initial
// POST /auth/login itself fails. Once a stepUpToken is received we commit
// to that base URL — never fall back mid-MFA (which would trigger a second SMS).
func (c *Client) loginLocked(ctx context.Context) error {
	slog.Info("verisure: logging in", "email", c.email)

	// Phase 1: POST /auth/login on each base until one accepts our credentials.
	type loginResult struct {
		base    string
		stepup  string // non-empty means MFA required
		cookies []*http.Cookie
	}

	var result *loginResult
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
			slog.Debug("verisure: login base failed", "base", base, "err", lastErr)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("POST /auth/login: HTTP %d: %s", resp.StatusCode, body)
			slog.Debug("verisure: login base rejected", "base", base, "status", resp.StatusCode)
			continue
		}

		r := &loginResult{base: base, cookies: resp.Cookies()}
		if strings.Contains(string(body), "stepUpToken") {
			for _, cookie := range resp.Cookies() {
				if cookie.Name == "vs-stepup" {
					r.stepup = cookie.Value
				}
			}
			if r.stepup == "" {
				lastErr = fmt.Errorf("MFA required but no vs-stepup cookie from %s", base)
				continue
			}
		}
		result = r
		break
	}

	if result == nil {
		return lastErr
	}

	// Propagate cookies to the API domain.
	c.propagateCookies(result.base, result.cookies)

	// Phase 2: complete MFA on the same base (no fallback — avoids double SMS).
	if result.stepup != "" {
		c.stepupCookie = result.stepup
		slog.Info("verisure: MFA required, triggering SMS")
		if err := c.mfaFlowLocked(ctx, result.base); err != nil {
			return fmt.Errorf("MFA flow: %w", err)
		}
	} else {
		slog.Info("verisure: login succeeded (no MFA)")
	}

	// Phase 3: discover installation GIID.
	if err := c.discoverGIIDLocked(ctx); err != nil {
		return fmt.Errorf("installation discovery: %w", err)
	}

	c.authed = true
	slog.Info("verisure: authenticated", "giid", c.giid)
	return nil
}

// mfaFlowLocked sends SMS, waits for the code, and validates it.
func (c *Client) mfaFlowLocked(ctx context.Context, loginBase string) error {
	// Drain stale code.
	select {
	case <-c.mfaCh:
	default:
	}

	// Trigger SMS.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginBase+"/auth/mfa", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "vs-stepup", Value: c.stepupCookie})
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

	// Wait for code.
	var code string
	select {
	case code = <-c.mfaCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Validate code: POST /auth/mfa/validate with JSON {"code": "XXXXXX"}.
	payload, _ := json.Marshal(mfaValidateRequest{Code: code})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost,
		loginBase+"/auth/mfa/validate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req2.Header.Set("APPLICATION_ID", applicationID)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json")
	req2.AddCookie(&http.Cookie{Name: "vs-stepup", Value: c.stepupCookie})

	resp2, err := c.http.Do(req2)
	if err != nil {
		return fmt.Errorf("POST /auth/mfa/validate: %w", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /auth/mfa/validate: HTTP %d: %s", resp2.StatusCode, body2)
	}

	// Propagate vid cookie from MFA validation response to API domain.
	c.propagateCookies(loginBase, resp2.Cookies())

	slog.Info("verisure: MFA code accepted")
	return nil
}

// discoverGIIDLocked fetches the installation list and populates c.giid.
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

// SetGIID pre-configures the installation ID (skips auto-discovery).
func (c *Client) SetGIID(giid string) {
	c.mu.Lock()
	c.giid = giid
	c.mu.Unlock()
}

// --- HTTP helpers ---

// get performs an authenticated GET and decodes the JSON response.
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

// setCookie injects a named cookie into the jar for the given base URL domain.
func (c *Client) setCookie(base, name, value string) {
	u, _ := url.Parse(base)
	c.jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value}})
}

// propagateCookies copies cookies received from the login server into the
// jar under both the login domain and the API base domain so that subsequent
// API calls include the vid cookie.
func (c *Client) propagateCookies(loginBase string, cookies []*http.Cookie) {
	loginURL, _ := url.Parse(loginBase)
	apiURL, _ := url.Parse(c.apiBase)

	// Set on login domain (the jar already does this via the HTTP client,
	// but we do it explicitly for the cookies we explicitly AddCookie'd).
	c.jar.SetCookies(loginURL, cookies)

	// Also set on the API base domain so API calls carry the vid.
	c.jar.SetCookies(apiURL, cookies)

	// And on the top-level verisure.com domain.
	verisureURL, _ := url.Parse("https://www.verisure.com")
	c.jar.SetCookies(verisureURL, cookies)
}
