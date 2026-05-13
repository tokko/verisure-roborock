package xiaomi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func authenticatedTestClient(t *testing.T, rt http.RoundTripper) *CloudClient {
	t.Helper()
	c, err := NewCloudClient("user@example.com", "password", "de")
	if err != nil {
		t.Fatal(err)
	}
	c.ssecurity = "MTIzNDU2Nzg5MDEyMzQ1Ng=="
	c.userId = "42"
	c.serviceToken = "token"
	c.http.Transport = rt
	return c
}

func TestRPCParsesJSONRPCResult(t *testing.T) {
	var gotPath string
	c := authenticatedTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		if cookie, err := req.Cookie("serviceToken"); err != nil || cookie.Value != "token" {
			t.Fatalf("serviceToken cookie missing: %v", err)
		}
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"data", "rc4_hash__", "signature", "ssecurity", "_nonce"} {
			if req.Form.Get(key) == "" {
				t.Fatalf("form missing %s", key)
			}
		}
		return response(`{"code":0,"result":{"id":7,"result":[{"state":5,"battery":88}]}}`), nil
	}))

	raw, err := c.RPC(context.Background(), "abc123", "get_status", 7, []any{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	if gotPath != "/app/home/rpc/abc123" {
		t.Fatalf("path = %q, want /app/home/rpc/abc123", gotPath)
	}
	var status []struct {
		State   int `json:"state"`
		Battery int `json:"battery"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status[0].State != 5 || status[0].Battery != 88 {
		t.Fatalf("status = %+v", status[0])
	}
}

func TestRPCParsesQuotedJSONRPCResult(t *testing.T) {
	c := authenticatedTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(`{"code":0,"result":"{\"id\":8,\"result\":[1,2,3]}"}`), nil
	}))

	raw, err := c.RPC(context.Background(), "abc123", "get_clean_summary", 8, []any{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	if string(raw) != "[1,2,3]" {
		t.Fatalf("result = %s, want [1,2,3]", raw)
	}
}

func TestDevicesUsesSharedRequestPath(t *testing.T) {
	c := authenticatedTestClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "de.api.io.mi.com" {
			t.Fatalf("host = %q, want de.api.io.mi.com", req.URL.Host)
		}
		if req.URL.Path != "/app/home/device_list" {
			t.Fatalf("path = %q, want /app/home/device_list", req.URL.Path)
		}
		return response(`{"code":0,"result":{"list":[{"did":"abc123","name":"upstairs","model":"roborock.vacuum.a10","localip":"192.0.2.10","token":"00112233445566778899aabbccddeeff"}]}}`), nil
	}))

	devices, err := c.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 1 || devices[0].DID != "abc123" || !devices[0].IsRoborock() {
		t.Fatalf("devices = %+v", devices)
	}
}

func TestAPIHostUsesChinaDefaultWithoutRegionSubdomain(t *testing.T) {
	if got := apiHost("cn"); got != "api.io.mi.com" {
		t.Fatalf("apiHost(cn) = %q, want api.io.mi.com", got)
	}
	if got := apiHost(""); got != "api.io.mi.com" {
		t.Fatalf("apiHost(empty) = %q, want api.io.mi.com", got)
	}
}

func TestNonceUsesXiaomiCloudShape(t *testing.T) {
	nonce := genNonce()
	raw, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		t.Fatalf("nonce is not base64: %v", err)
	}
	if len(raw) != 12 {
		t.Fatalf("nonce length = %d, want 12", len(raw))
	}
}

func TestSignDataUsesXiaomiSHA1Signature(t *testing.T) {
	got := signData("/home/device_list", map[string]string{
		"data":       `{"getVirtualModel":true}`,
		"rc4_hash__": "abc",
	}, "signed")
	const want = "WpfMeav/AlCfFyznG3SZduQ40xQ="
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestAuthCacheRoundTrip(t *testing.T) {
	path := t.TempDir() + "/xiaomi-auth.json"
	want := Auth{UserID: "42", SSecurity: "sec", ServiceToken: "token"}
	if err := SaveAuth(path, want); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	got, err := LoadAuth(path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if got.UserID != want.UserID || got.SSecurity != want.SSecurity || got.ServiceToken != want.ServiceToken {
		t.Fatalf("auth = %+v, want %+v", got, want)
	}

	c, err := NewCloudClient("user@example.com", "password", "de")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetAuth(got); err != nil {
		t.Fatalf("SetAuth: %v", err)
	}
	if !c.Auth().Valid() {
		t.Fatal("client auth is not valid")
	}
}

func TestSetAuthRejectsIncompleteAuth(t *testing.T) {
	c, err := NewCloudClient("user@example.com", "password", "de")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetAuth(Auth{UserID: "42"}); err == nil {
		t.Fatal("SetAuth succeeded with incomplete auth")
	}
}

func response(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
