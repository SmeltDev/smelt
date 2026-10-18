package grokauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/smeltdev/smelt/internal/harness/grok"
)

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func authJSON(expires string) string {
	return `{"https://auth.x.ai::u1":{"auth_mode":"oidc","key":"access-jwt","expires_at":` + expires +
		`,"refresh_token":"refresh-secret","email":"a@b.c","user_id":"u1"}}`
}

func quoted(t time.Time) string { return `"` + t.Format(time.RFC3339) + `"` }

func writeAuth(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.json")
	assert.NilError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func noEnv(string) string { return "" }

func TestContainerCopyBlanksRefreshTokens(t *testing.T) {
	cp, err := ContainerCopy([]byte(authJSON(quoted(now))))
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(string(cp), "refresh-secret"))
	var doc map[string]map[string]any
	assert.NilError(t, json.Unmarshal(cp, &doc))
	e := doc["https://auth.x.ai::u1"]
	assert.Equal(t, e["refresh_token"], "")
	assert.Equal(t, e["key"], "access-jwt")
	assert.Equal(t, e["email"], "a@b.c")
	assert.Equal(t, e["auth_mode"], "oidc")
	_, err = ContainerCopy([]byte("nope"))
	assert.Assert(t, err != nil)
}

func TestExpiry(t *testing.T) {
	want := now.Add(6 * time.Hour)
	tests := []struct {
		name, expires string
		want          time.Time
		wantErr       bool
	}{
		{"rfc3339", quoted(want), want, false},
		{"unix seconds", `1792339200`, time.Unix(1792339200, 0), false},
		{"unix milliseconds", `1792339200000`, time.Unix(1792339200, 0), false},
		{"numeric string", `"1792339200"`, time.Unix(1792339200, 0), false},
		{"garbage", `"soon"`, time.Time{}, true},
		{"missing", `null`, time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Expiry([]byte(authJSON(tt.expires)))
			if tt.wantErr {
				assert.Assert(t, err != nil)
				return
			}
			assert.NilError(t, err)
			assert.Assert(t, got.Equal(tt.want), "%v != %v", got, tt.want)
		})
	}
}

func TestExpiryIsTheLatestEntry(t *testing.T) {
	late := now.Add(5 * time.Hour)
	raw := `{"a":{"expires_at":` + quoted(now) + `},"b":{"expires_at":` + quoted(late) + `}}`
	got, err := Expiry([]byte(raw))
	assert.NilError(t, err)
	assert.Assert(t, got.Equal(late))
}

func TestForRunLeavesALongLoginAlone(t *testing.T) {
	ran := false
	h := Host{AuthPath: writeAuth(t, authJSON(quoted(now.Add(5*time.Hour)))), Env: noEnv,
		Run: func(context.Context, string, ...string) error { ran = true; return nil }}
	cred, err := h.ForRun(context.Background(), now)
	assert.NilError(t, err)
	assert.Assert(t, !ran, "no renewal when there is time left")
	assert.Equal(t, cred.EnvVar, grok.EnvAuthJSON)
	raw, err := base64.StdEncoding.DecodeString(cred.Value)
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(string(raw), "refresh-secret"))
}

func TestForRunRenewsAShortLogin(t *testing.T) {
	path := writeAuth(t, authJSON(quoted(now.Add(30*time.Minute))))
	var gotArgs []string
	h := Host{AuthPath: path, Binary: "grok-x", Env: noEnv,
		Run: func(_ context.Context, name string, args ...string) error {
			gotArgs = append([]string{name}, args...)
			return os.WriteFile(path, []byte(authJSON(quoted(now.Add(6*time.Hour)))), 0o600)
		}}
	_, err := h.ForRun(context.Background(), now)
	assert.NilError(t, err)
	assert.DeepEqual(t, gotArgs, []string{"grok-x", "models"})
}

func TestForRunReportsALoginThatCannotBeRenewed(t *testing.T) {
	short := authJSON(quoted(now.Add(time.Minute)))
	tests := []struct {
		name    string
		run     Runner
		wantMsg string
	}{
		{"renewal does nothing", func(context.Context, string, ...string) error { return nil }, ""},
		{"renewal fails", func(context.Context, string, ...string) error { return errors.New("offline") }, "offline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Host{AuthPath: writeAuth(t, short), Env: noEnv, Run: tt.run}
			_, err := h.ForRun(context.Background(), now)
			assert.ErrorIs(t, err, ErrExpired)
			if tt.wantMsg != "" {
				assert.ErrorContains(t, err, tt.wantMsg)
			}
			assert.Assert(t, !strings.Contains(err.Error(), "<nil>"))
		})
	}
}

func TestAPIKeyWinsOverALogin(t *testing.T) {
	h := Host{AuthPath: writeAuth(t, authJSON(quoted(now.Add(time.Minute)))),
		Env: func(k string) string {
			if k == grok.EnvAPIKey {
				return "xai-key"
			}
			return ""
		},
		Run: func(context.Context, string, ...string) error { t.Fatal("an API key needs no renewal"); return nil }}
	cred, err := h.ForRun(context.Background(), now)
	assert.NilError(t, err)
	assert.Equal(t, cred.EnvVar, grok.EnvAPIKey)
	assert.Equal(t, cred.Value, "xai-key")
}

func TestNoCredential(t *testing.T) {
	h := Host{AuthPath: filepath.Join(t.TempDir(), "missing.json"), Env: noEnv}
	_, err := h.ForRun(context.Background(), now)
	assert.ErrorIs(t, err, ErrNone)
	_, err = h.Find()
	assert.ErrorIs(t, err, ErrNone)
}

func TestExpiryAndForRunWithUnreadableSiblings(t *testing.T) {
	for _, sibling := range []string{`{"expires_at":"soon"}`, `{}`, `{"expires_at":null}`, `null`} {
		t.Run(sibling, func(t *testing.T) {
			late := now.Add(5 * time.Hour)
			raw := `{"good":{"expires_at":` + quoted(late) + `},"bad":` + sibling + `}`
			exp, err := Expiry([]byte(raw))
			assert.NilError(t, err)
			assert.Assert(t, exp.Equal(late))
			h := Host{AuthPath: writeAuth(t, raw), Env: noEnv, Run: func(context.Context, string, ...string) error {
				t.Fatal("must not renew a long readable login")
				return nil
			}}
			_, err = h.ForRun(t.Context(), now)
			assert.NilError(t, err)
		})
	}
	for _, raw := range []string{`{"a":{},"b":{"expires_at":"soon"}}`, `{"a":null}`} {
		_, err := Expiry([]byte(raw))
		assert.Assert(t, err != nil)
		h := Host{AuthPath: writeAuth(t, raw), Env: noEnv, Run: func(context.Context, string, ...string) error {
			t.Fatal("unreadable login must fail before renewal")
			return nil
		}}
		_, err = h.ForRun(t.Context(), now)
		assert.Assert(t, err != nil)
	}
	// A readable historical timestamp must not be mistaken for no readable value.
	exp, err := Expiry([]byte(`{"a":{},"b":{"expires_at":"0001-01-01T00:00:00Z"}}`))
	assert.NilError(t, err)
	assert.Assert(t, exp.IsZero())
}
