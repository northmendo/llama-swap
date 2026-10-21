package engineupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RemoteRuntime implements Runtime against a running llama-swap's agent
// API (the /agent/v1/engines/{name}/running and .../stop routes), so a
// CLI process swapping binaries outside the server can still see and
// stop the models using the engine. An unreachable server surfaces as
// an error, which the manager's gate treats as "cannot verify": the
// swap then requires force.
type RemoteRuntime struct {
	// BaseURL is the server's base, e.g. http://127.0.0.1:8080.
	BaseURL string

	// Key is the agent API bearer token.
	Key    string
	Client *http.Client
}

func (r RemoteRuntime) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return http.DefaultClient
}

// call performs one agent API request and decodes a JSON object body.
func (r RemoteRuntime) call(method, path string) (map[string]any, error) {
	req, err := http.NewRequest(method, strings.TrimRight(r.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Key)

	resp, err := r.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("server unreachable at %s: %w", r.BaseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server at %s answered %s: %s", r.BaseURL, resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("server at %s sent a malformed response: %w", r.BaseURL, err)
	}
	return parsed, nil
}

func stringsFromList(v any) []string {
	items, ok := v.([]any)
	if len(items) == 0 || !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (r RemoteRuntime) RunningModelsUsingEngine(engine string) ([]string, error) {
	body, err := r.call(http.MethodGet, "/agent/v1/engines/"+engine+"/running")
	if err != nil {
		return nil, err
	}
	return stringsFromList(body["runningModels"]), nil
}

func (r RemoteRuntime) StopModelsUsingEngine(engine string) ([]string, error) {
	body, err := r.call(http.MethodPost, "/agent/v1/engines/"+engine+"/stop")
	if err != nil {
		return nil, err
	}
	return stringsFromList(body["stopped"]), nil
}
