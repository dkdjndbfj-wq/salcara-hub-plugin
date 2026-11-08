package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"salcara/hub/internal/edgepool"
)

// Recovery uses only the independent local management channel. In particular it
// never depends on a working ingress route, environment proxy, DNS, or the UI.
func runRecovery(args []string, output io.Writer) error {
	if len(args) == 0 || args[0] != "state" && args[0] != "rollback" {
		return errors.New("usage: salcara-edge recover state|rollback --control-token-file PATH [--control-listen 127.0.0.1:8791] [--expected-revision N --trial-id ID|--version-id ID]")
	}
	action := args[0]
	flags := flag.NewFlagSet("recover "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	address := flags.String("control-listen", envOr("SALCARA_EDGE_CONTROL_LISTEN", "127.0.0.1:8791"), "numeric loopback management address")
	tokenFile := flags.String("control-token-file", envOr("SALCARA_EDGE_CONTROL_TOKEN_FILE", ""), "private control token file")
	revision := flags.Int64("expected-revision", 0, "required current configuration revision for rollback")
	trialID := flags.String("trial-id", "", "pending trial to roll back")
	versionID := flags.String("version-id", "", "saved version to restore")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return errors.New("invalid recovery arguments")
	}
	if err := loopback(*address); err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(*address)
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("control address requires a numeric TCP port")
	}
	if action == "rollback" && (*revision < 1 || (*trialID == "") == (*versionID == "")) {
		return errors.New("rollback requires expected-revision and exactly one trial-id or version-id")
	}
	if action == "state" && (*revision != 0 || *trialID != "" || *versionID != "") {
		return errors.New("state does not accept rollback arguments")
	}
	secret, err := readControlSecret(*tokenFile)
	if err != nil {
		return err
	}
	method, path := http.MethodGet, "/v1/state"
	var body io.Reader
	if action == "rollback" {
		method, path = http.MethodPost, "/v1/releases/rollback"
		b, _ := json.Marshal(struct {
			ExpectedRevision int64  `json:"expected_revision"`
			TrialID          string `json:"trial_id,omitempty"`
			VersionID        string `json:"version_id,omitempty"`
		}{*revision, *trialID, *versionID})
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+*address+path, body)
	if err != nil {
		return errors.New("invalid recovery address")
	}
	req.Header.Set("Authorization", "Bearer "+string(secret))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("local recovery channel is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recovery request rejected (HTTP %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("invalid recovery response")
	}
	var state struct {
		Revision int64                 `json:"revision"`
		Status   string                `json:"status"`
		Release  edgepool.ReleaseState `json:"release"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Revision < 1 {
		return errors.New("invalid recovery response")
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(state)
}
