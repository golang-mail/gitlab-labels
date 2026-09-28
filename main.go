// gitlab-labels scans email bodies/headers for GitLab issue URLs, fetches each
// referenced issue's labels via the GitLab API, and returns them as IMAP flag
// actions so the mail client (and other filters) can see the labels.
//
// Match shape: matches when at least one resolvable GitLab issue URL is found
// AND the API returns one or more labels. Returns one `flag` action per label
// (deduped, normalized with the configured prefix).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type initRequest struct {
	HostVersion string          `json:"host_version"`
	Config      json.RawMessage `json:"config,omitempty"`
}

type pluginAction struct {
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}

type pluginConfig struct {
	// Hosts the plugin should match (e.g. "gitlab.example.com").
	// If empty, matches any host whose URL path looks like a GitLab issue.
	Hosts []string `yaml:"hosts" json:"hosts"`
	// BaseURL overrides the API host (default: https://<host-from-url>).
	// Useful when emails reference a public hostname but the API lives elsewhere.
	BaseURL string `yaml:"base_url" json:"base_url"`
	// Token is a GitLab personal access token. Falls back to $GITLAB_TOKEN.
	Token string `yaml:"token" json:"token"`
	// LabelPrefix is prepended to each label flag (default "gitlab/").
	// Set to "" to use the raw label as the flag.
	LabelPrefix string `yaml:"label_prefix" json:"label_prefix"`
	// MaxIssues caps how many distinct issue URLs are resolved per email
	// (default 5) to bound API calls on huge digest emails.
	MaxIssues int `yaml:"max_issues" json:"max_issues"`
	// CacheTTL controls how long label lookups are cached in-process
	// (default 10m). Labels change rarely; this keeps a hot inbox cheap.
	CacheTTL time.Duration `yaml:"cache_ttl" json:"cache_ttl"`
	// HTTPTimeout per request (default 4s).
	HTTPTimeout time.Duration `yaml:"http_timeout" json:"http_timeout"`
	// Verbose logs each lookup. Off by default.
	Verbose bool `yaml:"verbose" json:"verbose"`
	// LabelStorage selects the action type the plugin emits:
	//   "local" (default) → `label` actions, stored in go-mail's UI Labels list
	//   "imap"            → `flag` actions, set as IMAP custom keywords on the
	//                        server (visible in Thunderbird/Apple Mail/etc.)
	// IMAP keywords have server-side character restrictions; the plugin already
	// collapses whitespace, but exotic labels (with `*%(){]"`) may silently fail
	// on stricter IMAP servers. Local is the safe default.
	LabelStorage string `yaml:"label_storage" json:"label_storage"`
	// RescanInterval (e.g. "1h", "30m") asks go-mail to periodically re-feed
	// every email in this plugin's filters' mailbox scope so newly-added GitLab
	// labels are picked up. Empty disables; default empty.
	RescanInterval string `yaml:"rescan_interval" json:"rescan_interval"`
	// IgnoreLabels is a list of GitLab label names that should NOT be applied to
	// emails (case-insensitive match against the raw GitLab label name, before
	// any rename or prefix). Useful for noisy labels like "wip" / "draft".
	IgnoreLabels []string `yaml:"ignore_labels" json:"ignore_labels"`
	// IgnorePrefixes drops any raw label starting with one of these prefixes
	// (case-insensitive). Handy for scoped labels such as "redmine::".
	IgnorePrefixes []string `yaml:"ignore_prefixes" json:"ignore_prefixes"`
	// Rename maps GitLab label names to the name used in flag/label actions
	// (case-insensitive keys, applied before LabelPrefix). E.g. {"my-super-label": "msl"}.
	Rename map[string]string `yaml:"rename" json:"rename"`
	// StripPrefixes drops a leading prefix from the label name when present
	// (case-insensitive, first match wins). E.g. ["backport-"] turns
	// "backport-v1.0" into "v1.0". Applied after Rename, before LabelPrefix.
	StripPrefixes []string `yaml:"strip_prefixes" json:"strip_prefixes"`
	// AddTargetBranch, when true, also emits the MR's target branch as a label.
	// Only applies to merge_requests refs (issues have no target branch). The
	// branch name goes through the same ignore/rename/strip_prefixes/prefix
	// pipeline as real labels, so you can e.g. rename "main" → "trunk".
	AddTargetBranch bool `yaml:"add_target_branch" json:"add_target_branch"`
	// AddKindLabel, when true, emits "mr" or "issue" as a label depending on
	// what the referenced URL points to. Useful for routing/coloring threads
	// without inspecting the body. Goes through the standard pipeline (rename
	// it to something else if you prefer different wording).
	AddKindLabel bool `yaml:"add_kind_label" json:"add_kind_label"`
	// StateActions maps a referenced ref's terminal state to an action emitted
	// IN ADDITION to label/flag actions — letting you auto-file notifications
	// once their issue/MR is done. Keys: "issue-closed", "mr-merged", "mr-closed"
	// (underscores also accepted). Action syntax: "move-to=>Mailbox", "delete",
	// "mark-read". Fires even when the ref carries no labels.
	StateActions map[string]string `yaml:"state_actions" json:"state_actions"`
}

type evalReq struct {
	Email struct {
		UID         uint32            `json:"uid"`
		Account     string            `json:"account"`
		Mailbox     string            `json:"mailbox"`
		Headers     map[string]string `json:"headers"`
		Body        string            `json:"body"`
		ContentType string            `json:"content_type"`
	} `json:"email"`
	FilterConfig string `json:"filter_config"`
}

var (
	cfg        pluginConfig
	httpClient *http.Client
	tokenInUse string

	cacheMu sync.Mutex
	cache   = map[string]cachedLabels{}

	// rateLimitedUntil is a process-wide cooldown: while it's in the future,
	// fetchLabels short-circuits without calling GitLab. Set when GitLab returns
	// 429/503, honoring Retry-After / RateLimit-Reset. Guarded by rateLimitMu.
	rateLimitMu      sync.Mutex
	rateLimitedUntil time.Time
)

// Backoff bounds for rate-limit handling.
const (
	defaultBackoff = 60 * time.Second
	maxBackoff     = 15 * time.Minute // clamp bogus/huge Retry-After values
)

// fetchConcurrency caps simultaneous GitLab lookups per evaluate, so total time
// is ~http_timeout rather than max_issues × http_timeout (which blew past the
// host's evaluate timeout and triggered restart loops).
const fetchConcurrency = 6

type cachedLabels struct {
	labels       []string
	targetBranch string // populated for merge_requests refs only
	state        string // GitLab ref state: opened/closed/merged/locked
	at           time.Time
}

func main() {
	log.SetFlags(0)
	path := os.Getenv("GO_MAIL_PLUGIN_SOCKET")
	if path == "" {
		log.Fatal("GO_MAIL_PLUGIN_SOCKET not set")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		log.Fatalf("listen %s: %v", path, err)
	}
	_ = os.Chmod(path, 0o600)
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serve(conn)
	}
}

func serve(conn net.Conn) {
	defer conn.Close()
	in := bufio.NewScanner(conn)
	in.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	out := json.NewEncoder(conn)

	for in.Scan() {
		var msg envelope
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			log.Printf("bad envelope: %v", err)
			continue
		}
		switch msg.Method {
		case "init":
			handleInit(out, msg)
		case "evaluate":
			handleEvaluate(out, msg)
		case "shutdown":
			return
		}
	}
}

func handleInit(out *json.Encoder, msg envelope) {
	cfg.LabelPrefix = "gitlab/"
	cfg.MaxIssues = 5
	cfg.CacheTTL = 10 * time.Minute
	cfg.HTTPTimeout = 4 * time.Second
	cfg.LabelStorage = "local"

	var req initRequest
	_ = json.Unmarshal(msg.Payload, &req)
	if len(req.Config) > 0 {
		if err := json.Unmarshal(req.Config, &cfg); err != nil {
			log.Printf("decode plugin config: %v", err)
		}
	}
	if cfg.LabelStorage == "" {
		cfg.LabelStorage = "local"
	}
	tokenInUse = cfg.Token
	if tokenInUse == "" {
		tokenInUse = os.Getenv("GITLAB_TOKEN")
	}
	if tokenInUse == "" {
		log.Printf("warning: no GitLab token configured (set config.token or $GITLAB_TOKEN); private issues will fail")
	}
	httpClient = &http.Client{Timeout: cfg.HTTPTimeout}

	// run_last=false: this plugin only reads and tags. Letting it run alongside
	// other matchers is safe and avoids stalling label-driven follow-on filters.
	runLast := false
	// Declare both action types — actual choice is per-evaluation based on
	// cfg.LabelStorage. The host uses this list for static documentation only.
	manifest, _ := json.Marshal(map[string]any{
		"name":            "gitlab-labels",
		"version":         "0.2.1",
		"actions":         []string{"label", "flag", "move", "delete", "mark_as_read"},
		"wants":           []string{"headers", "body"},
		"cacheable":       false, // we manage our own label cache with TTL; host caching would freeze the verdict
		"run_last":        runLast,
		"rerunnable":      true, // observer plugin: bypass host's run-once guard so re-filtering picks up upstream label changes
		"rescan_interval": cfg.RescanInterval,
	})
	_ = out.Encode(envelope{
		ID:      msg.ID,
		Type:    "response",
		Method:  "init",
		Payload: manifest,
	})
	log.Printf("initialized: hosts=%v prefix=%q max_issues=%d cache_ttl=%s token=%s label_storage=%s rescan=%s",
		cfg.Hosts, cfg.LabelPrefix, cfg.MaxIssues, cfg.CacheTTL, tokenPresent(tokenInUse), cfg.LabelStorage, cfg.RescanInterval)
}

func tokenPresent(t string) string {
	if t == "" {
		return "none"
	}
	return "set"
}

func handleEvaluate(out *json.Encoder, msg envelope) {
	var r evalReq
	if err := json.Unmarshal(msg.Payload, &r); err != nil {
		respond(out, msg.ID, map[string]any{"error": "decode evaluate: " + err.Error()})
		return
	}

	refs := extractIssueRefs(r.Email.Body, r.Email.Headers, cfg.Hosts, cfg.MaxIssues, cfg.Verbose, r.Email.UID)
	if len(refs) == 0 {
		respond(out, msg.ID, map[string]any{"matched": false})
		return
	}

	labelSet := map[string]struct{}{}
	ignoredCount := 0
	processOne := func(raw string) {
		if raw == "" || isIgnored(raw) {
			if raw != "" {
				ignoredCount++
			}
			return
		}
		labelSet[stripPrefix(renameLabel(raw))] = struct{}{}
	}
	// State-triggered actions deduped by type+value so N merged MRs in one
	// email don't yield N identical moves.
	stateActions := map[string]pluginAction{}

	// Fetch all refs concurrently (bounded), then process results in ref order.
	// Concurrency keeps total time ~http_timeout instead of N×http_timeout, so a
	// multi-link email can't blow past the host's evaluate timeout. Processing
	// stays single-threaded below, so labelSet/stateActions need no locking.
	results := fetchAllLabels(refs)
	for _, res := range results {
		ref := res.ref
		if res.err != nil {
			// Always logged — silently dropping labels was the original bug.
			log.Printf("uid=%d ref=%s lookup failed: %v", r.Email.UID, ref.key(), res.err)
			continue
		}
		labels, targetBranch, state := res.labels, res.targetBranch, res.state
		for _, raw := range labels {
			processOne(raw)
		}
		if cfg.AddTargetBranch && ref.Kind == "merge_requests" && targetBranch != "" {
			processOne(targetBranch)
		}
		if cfg.AddKindLabel {
			switch ref.Kind {
			case "merge_requests":
				processOne("mr")
			case "issues":
				processOne("issue")
			}
		}
		if spec, ok := stateActionFor(ref.Kind, state); ok {
			if act, ok := parseStateAction(spec); ok {
				stateActions[act.Type+"\x00"+act.Value] = act
			} else {
				log.Printf("uid=%d ref=%s invalid state_action %q (%s/%s)", r.Email.UID, ref.key(), spec, ref.Kind, state)
			}
		}
	}

	labels := make([]string, 0, len(labelSet))
	for l := range labelSet {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	if cfg.Verbose && ignoredCount > 0 {
		log.Printf("uid=%d ignored %d label(s) via ignore_labels", r.Email.UID, ignoredCount)
	}

	if len(labelSet) == 0 && len(stateActions) == 0 {
		if cfg.Verbose {
			log.Printf("uid=%d acct=%s/%s refs=%d labels=[] (no flags)",
				r.Email.UID, r.Email.Account, r.Email.Mailbox, len(refs))
		}
		respond(out, msg.ID, map[string]any{"matched": false, "reason": "no labels"})
		return
	}

	actionType := "label"
	if strings.EqualFold(cfg.LabelStorage, "imap") {
		actionType = "flag"
	}
	actions := make([]pluginAction, 0, len(labels)+len(stateActions))
	for _, l := range labels {
		actions = append(actions, pluginAction{Type: actionType, Value: flagName(l)})
	}
	actions = append(actions, sortedStateActions(stateActions)...)
	if cfg.Verbose {
		log.Printf("uid=%d acct=%s/%s refs=%d labels=%v flags=%v state_actions=%d",
			r.Email.UID, r.Email.Account, r.Email.Mailbox,
			len(refs), labels, actionValues(actions), len(stateActions))
	}
	respond(out, msg.ID, map[string]any{
		"matched": true,
		"actions": actions,
		"reason":  fmt.Sprintf("%d ref(s), %d label(s), %d state action(s)", len(refs), len(labels), len(stateActions)),
	})
}

// sortedStateActions returns the deduped state actions in a deterministic order
// (by type then value) so responses are stable across runs.
func sortedStateActions(m map[string]pluginAction) []pluginAction {
	out := make([]pluginAction, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// stateActionKey maps a ref's kind+state to the StateActions config key, or ""
// when the state isn't terminal/actionable. Only closed issues, merged MRs, and
// closed (un-merged) MRs trigger; opened/locked refs never do.
func stateActionKey(kind, state string) string {
	switch {
	case kind == "issues" && strings.EqualFold(state, "closed"):
		return "issue-closed"
	case kind == "merge_requests" && strings.EqualFold(state, "merged"):
		return "mr-merged"
	case kind == "merge_requests" && strings.EqualFold(state, "closed"):
		return "mr-closed"
	}
	return ""
}

// stateActionFor looks up the configured action spec for a ref's kind+state.
// Config keys are matched case-insensitively with "_" treated as "-".
func stateActionFor(kind, state string) (string, bool) {
	key := stateActionKey(kind, state)
	if key == "" || len(cfg.StateActions) == 0 {
		return "", false
	}
	for k, v := range cfg.StateActions {
		if strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), "_", "-")) == key {
			return v, true
		}
	}
	return "", false
}

// parseStateAction converts a state_actions value like "move-to=>Junk" into a
// host action. Verbs: move-to (alias move), delete, mark-read (alias
// mark_as_read). Returns ok=false for empty/unknown specs (caller logs).
func parseStateAction(spec string) (pluginAction, bool) {
	verb, arg := strings.TrimSpace(spec), ""
	if i := strings.Index(verb, "=>"); i >= 0 {
		arg = strings.TrimSpace(verb[i+2:])
		verb = strings.TrimSpace(verb[:i])
	}
	switch strings.ToLower(verb) {
	case "move-to", "move":
		if arg == "" {
			return pluginAction{}, false
		}
		return pluginAction{Type: "move", Value: arg}, true
	case "delete":
		return pluginAction{Type: "delete"}, true
	case "mark-read", "mark_as_read":
		return pluginAction{Type: "mark_as_read"}, true
	}
	return pluginAction{}, false
}

// flagName joins the configured prefix with the raw label and collapses any
// whitespace — IMAP keywords cannot contain spaces.
func flagName(label string) string {
	v := strings.TrimSpace(cfg.LabelPrefix + label)
	v = strings.ReplaceAll(v, " ", "_")
	return v
}

// isIgnored reports whether the raw GitLab label should be skipped entirely
// (case-insensitive exact match on IgnoreLabels or prefix match on IgnorePrefixes).
func isIgnored(raw string) bool {
	for _, ign := range cfg.IgnoreLabels {
		if strings.EqualFold(strings.TrimSpace(ign), raw) {
			return true
		}
	}
	lower := strings.ToLower(raw)
	for _, pre := range cfg.IgnorePrefixes {
		pre = strings.ToLower(strings.TrimSpace(pre))
		if pre != "" && strings.HasPrefix(lower, pre) {
			return true
		}
	}
	return false
}

// renameLabel returns the configured rename for `raw` or `raw` unchanged.
// Map keys are matched case-insensitively so the operator doesn't have to
// match GitLab's exact casing.
func renameLabel(raw string) string {
	if len(cfg.Rename) == 0 {
		return raw
	}
	for k, v := range cfg.Rename {
		if strings.EqualFold(strings.TrimSpace(k), raw) {
			return v
		}
	}
	return raw
}

// stripPrefix removes the first configured prefix that matches `raw` (case-insensitive).
// If stripping would yield an empty string the original is returned to avoid a useless empty action.
func stripPrefix(raw string) string {
	for _, p := range cfg.StripPrefixes {
		p = strings.TrimSpace(p)
		if p == "" || len(raw) < len(p) {
			continue
		}
		if strings.EqualFold(raw[:len(p)], p) {
			stripped := raw[len(p):]
			if stripped == "" {
				return raw
			}
			return stripped
		}
	}
	return raw
}

// issueRef identifies one GitLab issue or merge request across host + project path + iid.
type issueRef struct {
	Host    string
	Project string // e.g. "group/project"
	Kind    string // "issues" or "merge_requests" — matches the GitLab API path segment
	IID     string // iid as string (it's a path segment, not always small)
}

func (r issueRef) key() string { return r.Host + "|" + r.Project + "|" + r.Kind + "|" + r.IID }

// refURLRe captures GitLab issue OR merge request URLs of the form
//
//	https://<host>/<group>/<project>[/<subgroups...>]/-/(issues|merge_requests)/<iid>[#fragment]
//
// Both http(s) and mid-path '-' are required by GitLab's routing.
var refURLRe = regexp.MustCompile(`https?://([^/\s"'<>)]+)/([^\s"'<>)]+?)/-/(issues|merge_requests)/(\d+)`)

func extractIssueRefs(body string, headers map[string]string, allowedHosts []string, maxIssues int, verbose bool, uid uint32) []issueRef {
	if maxIssues <= 0 {
		maxIssues = 5
	}
	// Search body + a few headers that commonly carry the link (List-Post, References, In-Reply-To do not, but X-GitLab-* headers point to issue/MR URLs in some notification templates).
	corpus := body
	if len(headers) > 0 {
		for k, v := range headers {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "x-gitlab") || lk == "list-id" {
				corpus += "\n" + v
			}
		}
	}

	allowed := map[string]bool{}
	for _, h := range allowedHosts {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}

	matches := refURLRe.FindAllStringSubmatch(corpus, -1)

	seen := map[string]bool{}
	var out []issueRef
	rejectedHosts := map[string]int{}
	for _, m := range matches {
		host := strings.ToLower(m[1])
		project := strings.Trim(m[2], "/")
		kind := m[3]
		iid := m[4]
		if len(allowed) > 0 && !allowed[host] {
			rejectedHosts[host]++
			continue
		}
		ref := issueRef{Host: host, Project: project, Kind: kind, IID: iid}
		if seen[ref.key()] {
			continue
		}
		seen[ref.key()] = true
		out = append(out, ref)
		if len(out) >= maxIssues {
			break
		}
	}
	// Only ever logged when host filter actually rejected something — that's
	// the failure mode worth surfacing (misconfigured config.hosts).
	if verbose && len(rejectedHosts) > 0 {
		log.Printf("uid=%d rejected URLs by host (check config.hosts): %v", uid, rejectedHosts)
	}
	return out
}

func actionValues(actions []pluginAction) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Value)
	}
	return out
}

// fetchResult bundles one ref's lookup outcome for ordered processing after a
// concurrent fetch batch.
type fetchResult struct {
	ref          issueRef
	labels       []string
	targetBranch string
	state        string
	err          error
}

// fetchAllLabels resolves every ref concurrently (bounded by fetchConcurrency)
// and returns results in the same order as refs, so downstream processing stays
// deterministic. Each fetchLabels is self-synchronized (cache + rate-limit
// mutexes), so the only shared write here is each goroutine's own slot.
func fetchAllLabels(refs []issueRef) []fetchResult {
	results := make([]fetchResult, len(refs))
	sem := make(chan struct{}, fetchConcurrency)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref issueRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			labels, tb, state, err := fetchLabels(ref)
			results[i] = fetchResult{ref: ref, labels: labels, targetBranch: tb, state: state, err: err}
		}(i, ref)
	}
	wg.Wait()
	return results
}

// rateLimitRemaining returns how long the process-wide GitLab cooldown still
// has to run, or 0 when not currently rate-limited.
func rateLimitRemaining() time.Duration {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()
	if d := time.Until(rateLimitedUntil); d > 0 {
		return d
	}
	return 0
}

// enterBackoff arms the process-wide cooldown for d (clamped to maxBackoff),
// but never shortens an already-longer cooldown.
func enterBackoff(d time.Duration) {
	if d <= 0 {
		d = defaultBackoff
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	until := time.Now().Add(d)
	rateLimitMu.Lock()
	if until.After(rateLimitedUntil) {
		rateLimitedUntil = until
	}
	rateLimitMu.Unlock()
}

// parseRetryAfter derives a backoff duration from GitLab's rate-limit headers.
// Prefers Retry-After (delta-seconds or HTTP-date), then RateLimit-Reset (epoch
// seconds), falling back to defaultBackoff.
func parseRetryAfter(h http.Header) time.Duration {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(ra); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	if rr := strings.TrimSpace(h.Get("RateLimit-Reset")); rr != "" {
		if epoch, err := strconv.ParseInt(rr, 10, 64); err == nil {
			if d := time.Until(time.Unix(epoch, 0)); d > 0 {
				return d
			}
		}
	}
	return defaultBackoff
}

// fetchLabels returns the issue/MR labels, (for MRs) the target branch, and the
// ref's state (opened/closed/merged/locked). The target branch is empty for
// issues. All three are cached together so re-fetches don't drift.
func fetchLabels(ref issueRef) (labels []string, targetBranch, state string, err error) {
	k := ref.key()
	cacheMu.Lock()
	if v, ok := cache[k]; ok && time.Since(v.at) < cfg.CacheTTL {
		cacheMu.Unlock()
		return v.labels, v.targetBranch, v.state, nil
	}
	cacheMu.Unlock()

	// Skip the network entirely while rate-limited so evaluates stay fast and
	// we stop hammering GitLab until the limit window resets.
	if d := rateLimitRemaining(); d > 0 {
		return nil, "", "", fmt.Errorf("rate-limited, backing off %s", d.Round(time.Second))
	}

	base := cfg.BaseURL
	if base == "" {
		base = "https://" + ref.Host
	}
	kind := ref.Kind
	if kind == "" {
		kind = "issues"
	}
	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/%s/%s",
		strings.TrimRight(base, "/"),
		url.PathEscape(ref.Project),
		kind,
		url.PathEscape(ref.IID),
	)
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	if tokenInUse != "" {
		req.Header.Set("PRIVATE-TOKEN", tokenInUse)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	// 429 (rate limit) and 503 (overloaded) carry Retry-After/RateLimit-Reset —
	// arm the process-wide cooldown so we stop hammering GitLab until it clears.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		d := parseRetryAfter(resp.Header)
		enterBackoff(d)
		log.Printf("gitlab rate-limited (HTTP %d), backing off %s", resp.StatusCode, d.Round(time.Second))
		return nil, "", "", fmt.Errorf("gitlab API %s: backing off %s", resp.Status, d.Round(time.Second))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", "", fmt.Errorf("gitlab API %s: %s (url=%s)", resp.Status, strings.TrimSpace(string(body)), apiURL)
	}
	var payload struct {
		Labels       []string `json:"labels"`
		TargetBranch string   `json:"target_branch"`
		State        string   `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, "", "", fmt.Errorf("decode issue: %w", err)
	}

	cacheMu.Lock()
	cache[k] = cachedLabels{labels: payload.Labels, targetBranch: payload.TargetBranch, state: payload.State, at: time.Now()}
	cacheMu.Unlock()
	return payload.Labels, payload.TargetBranch, payload.State, nil
}

func respond(out *json.Encoder, id string, payload any) {
	raw, _ := json.Marshal(payload)
	_ = out.Encode(envelope{
		ID:      id,
		Type:    "response",
		Method:  "evaluate",
		Payload: raw,
	})
}
