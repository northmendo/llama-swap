// Tests for the config verbs: the safe state view, the peer/selector/
// agent_api write pipeline (dry run, apply, every reject path) and the
// backup list/restore pair.
package agentapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// peerAndSelectorYAML appends a peer with an ${env.*} key macro and a
// failover selector targeting it to the base test config.
const peerAndSelectorYAML = `
peers:
  upstream:
    proxy: http://127.0.0.1:9393
    apiKey: ${env.PEER_UPSTREAM_KEY}
    models:
      - remote-model

selectors:
  cascade:
    strategy: failover
    targets:
      - existing
      - upstream/remote-model
`

// envWithPeer builds the standard test env with the peer and selector
// above, and the macro's env var set.
func envWithPeer(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("PEER_UPSTREAM_KEY", "resolved-peer-secret-8613")
	return newTestEnv(t, func(y string) string { return y + peerAndSelectorYAML })
}

// fileText returns the current config file content.
func fileText(t *testing.T, env *testEnv) string {
	t.Helper()
	raw, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	return string(raw)
}

// decoded as map[string]any
func bodyMap(t *testing.T, payload string) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(payload), &out))
	return out
}

// writeResponse captures the write-verb wire shape for assertions.
type writeResponse struct {
	Status         string          `json:"status"`
	Approved       bool            `json:"approved"`
	Written        bool            `json:"written"`
	ReloadRequired bool            `json:"reloadRequired"`
	Backup         string          `json:"backup"`
	Message        string          `json:"message"`
	Config         *configStateDTO `json:"config"`
}

// postVerb performs an authenticated write verb and decodes the response.
func postVerb(t *testing.T, env *testEnv, path string, body any) (int, writeResponse) {
	t.Helper()
	status, payload := env.do(t, http.MethodPost, path, body, testKey)
	resp := writeResponse{}
	require.NoError(t, json.Unmarshal([]byte(payload), &resp))
	return status, resp
}

// peerFromState finds one peer in a state DTO.
func peerFromState(t *testing.T, state *configStateDTO, name string) *peerStateDTO {
	t.Helper()
	for i := range state.Peers {
		if state.Peers[i].Name == name {
			return &state.Peers[i]
		}
	}
	t.Fatalf("peer %q not in state", name)
	return nil
}

// selectorFromState finds one selector in a state DTO.
func selectorFromState(t *testing.T, state *configStateDTO, name string) *selectorStateDTO {
	t.Helper()
	for i := range state.Selectors {
		if state.Selectors[i].Name == name {
			return &state.Selectors[i]
		}
	}
	t.Fatalf("selector %q not in state", name)
	return nil
}

// ---------------------------------------------------------------------------
// GET /agent/v1/config
// ---------------------------------------------------------------------------

func TestConfigState_ShowsMacrosNeverSecrets(t *testing.T) {
	env := envWithPeer(t)

	var state configStateDTO
	env.getOK(t, http.MethodGet, "/agent/v1/config", nil, &state)

	peer := peerFromState(t, &state, "upstream")
	assert.Equal(t, "http://127.0.0.1:9393", peer.Proxy)
	assert.Equal(t, []string{"remote-model"}, peer.Models)
	assert.True(t, peer.ApiKey.Set)
	assert.Equal(t, "${env.PEER_UPSTREAM_KEY}", peer.ApiKey.Macro)

	sel := selectorFromState(t, &state, "cascade")
	assert.Equal(t, "failover", sel.Strategy)
	assert.Equal(t, []string{"existing", "upstream/remote-model"}, sel.Targets)

	require.NotNil(t, state.AgentAPI)
	assert.True(t, state.AgentAPI.Enabled)
	assert.True(t, state.AgentAPI.Key.Set)
	// The configured key is a literal in the fixture, so no macro is
	// reported — and the literal itself must never travel.
	assert.Empty(t, state.AgentAPI.Key.Macro)

	// The resolved peer secret and the agent API key never appear.
	status, payload := env.do(t, http.MethodGet, "/agent/v1/config", nil, testKey)
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, payload, "resolved-peer-secret-8613")
	assert.NotContains(t, payload, testKey)
}

func TestConfigState_LiteralPeerKeyShowsSetOnly(t *testing.T) {
	env := newTestEnv(t, func(y string) string {
		return y + `
peers:
  literalpeer:
    proxy: http://127.0.0.1:9394
    apiKey: literal-peer-secret-9731
    models:
      - some-model
`
	})

	status, payload := env.do(t, http.MethodGet, "/agent/v1/config", nil, testKey)
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, payload, "literal-peer-secret-9731")
	assert.Contains(t, payload, `"name":"literalpeer"`)
	assert.Contains(t, payload, `"apiKey":{"set":true}`)
}

// ---------------------------------------------------------------------------
// peers.add
// ---------------------------------------------------------------------------

func TestPeerAdd_DryRunThenApply(t *testing.T) {
	t.Setenv("PEER_NEW_KEY", "whatever")
	env := newTestEnv(t, nil)
	before := fileText(t, env)

	// Dry run: valid, pending, nothing written.
	status, resp := postVerb(t, env, "/agent/v1/config/peers.add", map[string]any{
		"name":   "openrouter",
		"proxy":  "https://openrouter.ai/api",
		"apiKey": "${env.PEER_NEW_KEY}",
		"models": []string{"llama-3.1-8b"},
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "pending", resp.Status)
	assert.False(t, resp.Written)
	assert.Empty(t, resp.Backup)
	require.NotNil(t, resp.Config)
	peer := peerFromState(t, resp.Config, "openrouter")
	assert.Equal(t, "${env.PEER_NEW_KEY}", peer.ApiKey.Macro)
	assert.Equal(t, before, fileText(t, env), "dry run must not touch the file")

	// Apply: written, backup taken, stanza present, comment survives.
	status, resp = postVerb(t, env, "/agent/v1/config/peers.add", map[string]any{
		"approved": true,
		"name":     "openrouter",
		"proxy":    "https://openrouter.ai/api",
		"apiKey":   "${env.PEER_NEW_KEY}",
		"models":   []string{"llama-3.1-8b"},
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "applied", resp.Status)
	assert.True(t, resp.Written)
	assert.True(t, resp.ReloadRequired, "tests mount without watchConfig")
	require.NotEmpty(t, resp.Backup)
	assert.FileExists(t, resp.Backup)

	after := fileText(t, env)
	assert.Contains(t, after, "openrouter:")
	assert.Contains(t, after, "https://openrouter.ai/api")
	assert.Contains(t, after, "${env.PEER_NEW_KEY}")
	assert.Contains(t, after, "# a comment that must survive an apply")
	assert.NotContains(t, after, "PEER_NEW_KEY_RESOLVED")

	// The backup holds the pre-change bytes.
	backupRaw, err := os.ReadFile(resp.Backup)
	require.NoError(t, err)
	assert.Equal(t, before, string(backupRaw))
}

func TestPeerAdd_RejectsLiteralKey(t *testing.T) {
	env := newTestEnv(t, nil)

	status, respBody := env.do(t, http.MethodPost, "/agent/v1/config/peers.add", map[string]any{
		"approved": true,
		"name":     "badpeer",
		"proxy":    "http://127.0.0.1:9395",
		"apiKey":   "sk-literal-secret-123",
		"models":   []string{"m"},
	}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, respBody, "must reference an environment variable macro")
	assert.NotContains(t, fileText(t, env), "badpeer")
}

func TestPeerAdd_FieldValidation(t *testing.T) {
	env := newTestEnv(t, nil)

	cases := []struct {
		label string
		body  map[string]any
		want  string
	}{
		{"no name", map[string]any{"proxy": "http://x", "models": []string{"m"}}, `name`},
		{"no proxy", map[string]any{"name": "p", "models": []string{"m"}}, `proxy`},
		{"no models", map[string]any{"name": "p", "proxy": "http://x"}, `at least one entry`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.add", tc.body, testKey)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Contains(t, body, tc.want)
		})
	}
}

func TestPeerAdd_DuplicateIsConflict(t *testing.T) {
	env := envWithPeer(t)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.add", map[string]any{
		"approved": true,
		"name":     "upstream",
		"proxy":    "http://127.0.0.1:9396",
		"models":   []string{"m"},
	}, testKey)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, body, "already defined")
}

func TestPeerAdd_UnsetEnvMacroFailsValidation(t *testing.T) {
	env := newTestEnv(t, nil)
	before := fileText(t, env)

	// PEER_NEVER_SET is not exported in the test process, so the merged
	// document cannot load and the validator's message comes back.
	status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.add", map[string]any{
		"approved": true,
		"name":     "ghostmacro",
		"proxy":    "http://127.0.0.1:9397",
		"apiKey":   "${env.PEER_NEVER_SET}",
		"models":   []string{"m"},
	}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body, "PEER_NEVER_SET")
	assert.Equal(t, before, fileText(t, env), "failed validation must not touch the file")
}

func TestPeerAdd_NeedsSingleConfigFile(t *testing.T) {
	t.Setenv("PEER_ANY_KEY", "x")
	env := newTestEnv(t, nil)
	server := mountServer(t, Options{Cfg: env.cfg, ConfigPath: nil})

	status, body := postJSON(t, server, "/agent/v1/config/peers.add", map[string]any{
		"approved": true,
		"name":     "p",
		"proxy":    "http://127.0.0.1:9398",
		"models":   []string{"m"},
	})
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, body, "single config file")
}

// ---------------------------------------------------------------------------
// peers.update / peers.remove
// ---------------------------------------------------------------------------

func TestPeerUpdate_OverlaysFields(t *testing.T) {
	env := envWithPeer(t)

	status, resp := postVerb(t, env, "/agent/v1/config/peers.update", map[string]any{
		"approved": true,
		"name":     "upstream",
		"proxy":    "http://127.0.0.1:9999",
		"models":   []string{"remote-model", "second-model"},
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "applied", resp.Status)
	peer := peerFromState(t, resp.Config, "upstream")
	assert.Equal(t, "http://127.0.0.1:9999", peer.Proxy)
	assert.Equal(t, []string{"remote-model", "second-model"}, peer.Models)
	// Untouched field survives.
	assert.Equal(t, "${env.PEER_UPSTREAM_KEY}", peer.ApiKey.Macro)

	after := fileText(t, env)
	assert.Contains(t, after, "http://127.0.0.1:9999")
	assert.Contains(t, after, "- second-model")
	assert.Contains(t, after, "${env.PEER_UPSTREAM_KEY}")
}

func TestPeerUpdate_ClearsKeyWithEmptyMacro(t *testing.T) {
	env := envWithPeer(t)

	status, resp := postVerb(t, env, "/agent/v1/config/peers.update", map[string]any{
		"approved": true,
		"name":     "upstream",
		"apiKey":   "",
	})
	require.Equal(t, http.StatusOK, status)
	peer := peerFromState(t, resp.Config, "upstream")
	assert.False(t, peer.ApiKey.Set)
	assert.NotContains(t, fileText(t, env), "PEER_UPSTREAM_KEY")
}

func TestPeerUpdate_RejectsLiteralKey(t *testing.T) {
	env := envWithPeer(t)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.update", map[string]any{
		"approved": true,
		"name":     "upstream",
		"apiKey":   "literal-nope",
	}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body, "macro")
}

func TestPeerUpdate_UnknownPeerIs404(t *testing.T) {
	env := newTestEnv(t, nil)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.update", map[string]any{
		"approved": true,
		"name":     "ghost",
		"proxy":    "http://127.0.0.1:9",
	}, testKey)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "peers.ghost")
}

func TestPeerRemove_DryRunReportsValidation(t *testing.T) {
	env := envWithPeer(t)
	before := fileText(t, env)

	// The dry run of an invalid change reports the validator's exact
	// message instead of "pending" — nothing is written either way.
	status, payload := env.do(t, http.MethodPost, "/agent/v1/config/peers.remove", map[string]any{"name": "upstream"}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, payload, "references unknown model")
	assert.Equal(t, before, fileText(t, env))
}

func TestPeerRemove_RejectsPeerReferencedBySelector(t *testing.T) {
	env := envWithPeer(t)
	before := fileText(t, env)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.remove", map[string]any{
		"approved": true,
		"name":     "upstream",
	}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body, "references unknown model")
	assert.Contains(t, body, "upstream/remote-model")
	assert.Equal(t, before, fileText(t, env), "rejected removal must not touch the file")
}

func TestPeerRemove_SuccessDropsEmptySection(t *testing.T) {
	env := newTestEnv(t, func(y string) string {
		return y + `
peers:
  onlypeer:
    proxy: http://127.0.0.1:9393
    models:
      - remote-model
`
	})

	status, resp := postVerb(t, env, "/agent/v1/config/peers.remove", map[string]any{
		"approved": true,
		"name":     "onlypeer",
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "applied", resp.Status)
	after := fileText(t, env)
	assert.NotContains(t, after, "onlypeer")
	assert.NotContains(t, after, "peers:")
}

func TestPeerRemove_UnknownPeerIs404(t *testing.T) {
	env := newTestEnv(t, nil)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/peers.remove", map[string]any{
		"approved": true,
		"name":     "ghost",
	}, testKey)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "peers.ghost")
}

// ---------------------------------------------------------------------------
// selectors.add / update / remove
// ---------------------------------------------------------------------------

func TestSelectorAdd_FailoverApplied(t *testing.T) {
	env := envWithPeer(t)

	status, resp := postVerb(t, env, "/agent/v1/config/selectors.add", map[string]any{
		"approved": true,
		"name":     "chain",
		"strategy": "failover",
		"targets":  []string{"existing", "upstream/remote-model"},
		"cooldown": "45s",
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "applied", resp.Status)
	sel := selectorFromState(t, resp.Config, "chain")
	assert.Equal(t, "failover", sel.Strategy)
	assert.Equal(t, []string{"existing", "upstream/remote-model"}, sel.Targets)
	assert.Equal(t, "45s", sel.Cooldown)

	after := fileText(t, env)
	assert.Contains(t, after, "chain:")
	assert.Contains(t, after, "strategy: failover")
	assert.Contains(t, after, "cooldown: 45s")

	// The written selector must load through the real pipeline.
	_, err := config.LoadConfig(env.cfgPath)
	require.NoError(t, err)
}

func TestSelectorAdd_RejectPaths(t *testing.T) {
	env := envWithPeer(t)
	before := fileText(t, env)

	cases := []struct {
		label string
		body  map[string]any
		code  int
		want  string
	}{
		{"dangling target", map[string]any{
			"approved": true, "name": "s1", "strategy": "failover",
			"targets": []string{"ghost-model"},
		}, http.StatusBadRequest, "references unknown model"},
		{"selector chaining", map[string]any{
			"approved": true, "name": "s2", "strategy": "failover",
			"targets": []string{"cascade"},
		}, http.StatusBadRequest, "selector chaining is not supported"},
		{"duplicate name", map[string]any{
			"approved": true, "name": "cascade", "strategy": "failover",
			"targets": []string{"existing"},
		}, http.StatusConflict, "already defined"},
		{"unknown strategy", map[string]any{
			"approved": true, "name": "s3", "strategy": "bogus",
			"targets": []string{"existing"},
		}, http.StatusBadRequest, "unknown strategy"},
		{"warm needs local", map[string]any{
			"approved": true, "name": "s4", "strategy": "warm",
			"targets": []string{"upstream/remote-model"},
		}, http.StatusBadRequest, "must resolve to a local model"},
		{"no name", map[string]any{
			"strategy": "failover", "targets": []string{"existing"},
		}, http.StatusBadRequest, "is required"},
		{"no strategy", map[string]any{
			"approved": true, "name": "s5", "targets": []string{"existing"},
		}, http.StatusBadRequest, "warm, pin, spillover, failover"},
		{"no targets", map[string]any{
			"approved": true, "name": "s6", "strategy": "failover",
		}, http.StatusBadRequest, `at least one entry`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			status, body := env.do(t, http.MethodPost, "/agent/v1/config/selectors.add", tc.body, testKey)
			assert.Equal(t, tc.code, status, "body: %s", body)
			assert.Contains(t, body, tc.want)
		})
	}
	assert.Equal(t, before, fileText(t, env), "reject paths must not touch the file")
}

func TestSelectorUpdate_ReordersTargets(t *testing.T) {
	env := envWithPeer(t)

	status, resp := postVerb(t, env, "/agent/v1/config/selectors.update", map[string]any{
		"approved": true,
		"name":     "cascade",
		"targets":  []string{"upstream/remote-model", "existing"},
		"cooldown": "2m",
	})
	require.Equal(t, http.StatusOK, status)
	sel := selectorFromState(t, resp.Config, "cascade")
	assert.Equal(t, []string{"upstream/remote-model", "existing"}, sel.Targets)
	assert.Equal(t, "2m", sel.Cooldown)

	after := fileText(t, env)
	upstreamPos := strings.Index(after, "- upstream/remote-model")
	existingPos := strings.Index(after, "- existing")
	require.GreaterOrEqual(t, upstreamPos, 0)
	require.GreaterOrEqual(t, existingPos, 0)
	assert.Less(t, upstreamPos, existingPos, "upstream must now come first")
}

func TestSelectorUpdate_UnknownIs404(t *testing.T) {
	env := newTestEnv(t, nil)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/selectors.update", map[string]any{
		"approved": true, "name": "ghost", "strategy": "failover",
	}, testKey)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "selectors.ghost")
}

func TestSelectorRemove_Success(t *testing.T) {
	env := envWithPeer(t)

	status, resp := postVerb(t, env, "/agent/v1/config/selectors.remove", map[string]any{
		"approved": true,
		"name":     "cascade",
	})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "applied", resp.Status)
	after := fileText(t, env)
	assert.NotContains(t, after, "cascade:")
	assert.NotContains(t, after, "selectors:")
}

func TestSelectorRemove_UnknownIs404(t *testing.T) {
	env := newTestEnv(t, nil)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/selectors.remove", map[string]any{
		"approved": true, "name": "ghost",
	}, testKey)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, "selectors.ghost")
}

// ---------------------------------------------------------------------------
// agent_api.update
// ---------------------------------------------------------------------------

func TestAgentAPIUpdate_RejectsLiteralKey(t *testing.T) {
	env := newTestEnv(t, nil)
	before := fileText(t, env)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/agent_api.update", map[string]any{
		"approved": true,
		"key":      "sk-agent-literal-789",
	}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body, "macro")
	assert.NotContains(t, body, "sk-agent-literal-789")
	assert.Equal(t, before, fileText(t, env))
}

func TestAgentAPIUpdate_KeyMacroAndToggle(t *testing.T) {
	t.Setenv("NEW_AGENT_KEY", "agent-key-value")
	env := newTestEnv(t, nil)

	// Rotate to a macro reference.
	status, resp := postVerb(t, env, "/agent/v1/config/agent_api.update", map[string]any{
		"approved": true,
		"key":      "${env.NEW_AGENT_KEY}",
	})
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, resp.Config.AgentAPI)
	assert.True(t, resp.Config.AgentAPI.Key.Set)
	assert.Equal(t, "${env.NEW_AGENT_KEY}", resp.Config.AgentAPI.Key.Macro)
	assert.Contains(t, fileText(t, env), "${env.NEW_AGENT_KEY}")

	// Disable, and the response must warn the routes unmount on reload.
	status, resp = postVerb(t, env, "/agent/v1/config/agent_api.update", map[string]any{
		"approved": true,
		"enabled":  false,
	})
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, resp.Message, "unmounted")
	require.NotNil(t, resp.Config.AgentAPI)
	assert.False(t, resp.Config.AgentAPI.Enabled)

	// Clearing the key is only valid while the section is disabled
	// (validation requires a key when enabled); it drops from the file.
	status, resp = postVerb(t, env, "/agent/v1/config/agent_api.update", map[string]any{
		"approved": true,
		"key":      "",
	})
	require.Equal(t, http.StatusOK, status)
	after := fileText(t, env)
	assert.Contains(t, after, "enabled: false")
	assert.NotContains(t, after, "NEW_AGENT_KEY")
}

func TestAgentAPIUpdate_NothingToUpdateIs400(t *testing.T) {
	env := newTestEnv(t, nil)

	status, body := env.do(t, http.MethodPost, "/agent/v1/config/agent_api.update", map[string]any{
		"approved": true,
	}, testKey)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, body, "nothing to update")
}

// ---------------------------------------------------------------------------
// backups + restore
// ---------------------------------------------------------------------------

func TestConfigBackups_ListsNewestFirst(t *testing.T) {
	t.Setenv("PEER_BK_KEY", "x")
	env := newTestEnv(t, nil)

	// No backups yet.
	var listed struct {
		Backups []struct {
			Name      string `json:"name"`
			Path      string `json:"path"`
			SizeBytes int64  `json:"sizeBytes"`
		} `json:"backups"`
	}
	env.getOK(t, http.MethodGet, "/agent/v1/config/backups", nil, &listed)
	assert.Empty(t, listed.Backups)

	// Two writes -> two backups.
	for _, name := range []string{"peer-one", "peer-two"} {
		status, resp := postVerb(t, env, "/agent/v1/config/peers.add", map[string]any{
			"approved": true,
			"name":     name,
			"proxy":    "http://127.0.0.1:9400",
			"models":   []string{"m"},
		})
		require.Equal(t, http.StatusOK, status)
		require.NotEmpty(t, resp.Backup)
	}

	env.getOK(t, http.MethodGet, "/agent/v1/config/backups", nil, &listed)
	require.Len(t, listed.Backups, 2)
	assert.True(t, strings.HasPrefix(listed.Backups[0].Name, "config.yaml.bak-"),
		"unexpected backup name %q", listed.Backups[0].Name)
	assert.Greater(t, listed.Backups[0].SizeBytes, int64(0))
	// Names sort chronologically; newest first.
	assert.Greater(t, listed.Backups[0].Name, listed.Backups[1].Name)
}

func TestConfigRestore_Roundtrip(t *testing.T) {
	t.Setenv("PEER_RT_KEY", "x")
	env := newTestEnv(t, nil)
	original := fileText(t, env)

	// One approved write takes a backup of the pre-change config.
	status, resp := postVerb(t, env, "/agent/v1/config/peers.add", map[string]any{
		"approved": true,
		"name":     "roundtripper",
		"proxy":    "http://127.0.0.1:9401",
		"models":   []string{"m"},
	})
	require.Equal(t, http.StatusOK, status)
	changed := fileText(t, env)
	require.NotEqual(t, original, changed)

	// Dry run restores nothing.
	backupName := filepath.Base(resp.Backup)
	status, rbody := env.do(t, http.MethodPost, "/agent/v1/config/restore", map[string]any{
		"backup": backupName,
	}, testKey)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, rbody, "pending")
	assert.Equal(t, changed, fileText(t, env))

	// Approved restore puts the original bytes back.
	status, rbody = env.do(t, http.MethodPost, "/agent/v1/config/restore", map[string]any{
		"approved": true,
		"backup":   backupName,
	}, testKey)
	require.Equal(t, http.StatusOK, status)
	out := bodyMap(t, rbody)
	assert.Equal(t, "applied", out["status"])
	assert.Equal(t, backupName, filepath.Base(fmt.Sprint(out["restoredFrom"])))
	assert.Contains(t, out["message"], "reload")
	assert.Equal(t, original, fileText(t, env))

	// The restore itself took a safety backup of the replaced config.
	var listed struct {
		Backups []struct {
			Name string `json:"name"`
		} `json:"backups"`
	}
	env.getOK(t, http.MethodGet, "/agent/v1/config/backups", nil, &listed)
	require.Len(t, listed.Backups, 2)
}

func TestConfigRestore_RejectPaths(t *testing.T) {
	env := newTestEnv(t, nil)

	cases := []struct {
		label  string
		backup string
		code   int
		want   string
	}{
		{"missing name", "", http.StatusBadRequest, "is required"},
		{"path traversal", "../../etc/passwd", http.StatusBadRequest, "not a path"},
		{"unknown backup", "config.yaml.bak-20990101T000000Z", http.StatusNotFound, "unknown backup"},
		{"not a backup name", "config.yaml", http.StatusNotFound, "unknown backup"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			status, body := env.do(t, http.MethodPost, "/agent/v1/config/restore", map[string]any{
				"approved": true,
				"backup":   tc.backup,
			}, testKey)
			assert.Equal(t, tc.code, status, "body: %s", body)
			assert.Contains(t, body, tc.want)
		})
	}
}
