package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExtractIssueRefs(t *testing.T) {
	body := `Hello,

See https://gitlab.internal.example/group/project/-/issues/108#note_341321 for context.
Also relevant: https://gitlab.internal.example/group/project/-/issues/108 (dupe)
And a nested group: https://gitlab.com/group/sub/proj/-/issues/42
This is unrelated: https://example.com/foo/-/issues/1
`
	refs := extractIssueRefs(body, nil, []string{"gitlab.internal.example", "gitlab.com"}, 10, false, 0)
	want := []issueRef{
		{Host: "gitlab.internal.example", Project: "group/project", Kind: "issues", IID: "108"},
		{Host: "gitlab.com", Project: "group/sub/proj", Kind: "issues", IID: "42"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("got %#v want %#v", refs, want)
	}
}

func TestExtractIssueRefsAllowHostFilter(t *testing.T) {
	body := "see https://gitlab.com/g/p/-/issues/1 and https://gitlab.internal.example/g/p/-/issues/2"
	refs := extractIssueRefs(body, nil, []string{"gitlab.internal.example"}, 10, false, 0)
	if len(refs) != 1 || refs[0].Host != "gitlab.internal.example" || refs[0].IID != "2" {
		t.Fatalf("expected single internal host ref, got %#v", refs)
	}
}

func TestExtractIssueRefsNoHostFilter(t *testing.T) {
	body := "https://anywhere.example/g/p/-/issues/9"
	refs := extractIssueRefs(body, nil, nil, 10, false, 0)
	if len(refs) != 1 || refs[0].Host != "anywhere.example" {
		t.Fatalf("expected to keep all hosts when filter empty, got %#v", refs)
	}
}

func TestExtractIssueRefsMaxIssues(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("https://gitlab.example/g/p/-/issues/")
		b.WriteString(itoa(i))
		b.WriteByte('\n')
	}
	refs := extractIssueRefs(b.String(), nil, nil, 3, false, 0)
	if len(refs) != 3 {
		t.Fatalf("max_issues not honored: got %d want 3", len(refs))
	}
}

func TestExtractMergeRequestRefs(t *testing.T) {
	body := `Review needed: https://gitlab.example/group/proj/-/merge_requests/55
And a fix MR: https://gitlab.example/group/proj/-/merge_requests/56#note_1
Linked issue: https://gitlab.example/group/proj/-/issues/77`
	refs := extractIssueRefs(body, nil, nil, 10, false, 0)
	want := []issueRef{
		{Host: "gitlab.example", Project: "group/proj", Kind: "merge_requests", IID: "55"},
		{Host: "gitlab.example", Project: "group/proj", Kind: "merge_requests", IID: "56"},
		{Host: "gitlab.example", Project: "group/proj", Kind: "issues", IID: "77"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("got %#v want %#v", refs, want)
	}
}

// An issue and an MR with the same iid must NOT dedupe to a single ref —
// they live on different API endpoints and may carry different labels.
func TestIssueAndMergeRequestSameIIDAreDistinct(t *testing.T) {
	body := `https://gitlab.example/g/p/-/issues/10
https://gitlab.example/g/p/-/merge_requests/10`
	refs := extractIssueRefs(body, nil, nil, 10, false, 0)
	if len(refs) != 2 {
		t.Fatalf("expected 2 distinct refs, got %#v", refs)
	}
	kinds := map[string]bool{refs[0].Kind: true, refs[1].Kind: true}
	if !kinds["issues"] || !kinds["merge_requests"] {
		t.Fatalf("expected both kinds present, got %#v", refs)
	}
}

func TestExtractIssueRefsFromHeaders(t *testing.T) {
	headers := map[string]string{
		"X-GitLab-Project":    "group/project",
		"X-GitLab-Issue-Path": "https://gitlab.internal.example/group/project/-/issues/777",
	}
	refs := extractIssueRefs("", headers, []string{"gitlab.internal.example"}, 10, false, 0)
	if len(refs) != 1 || refs[0].IID != "777" {
		t.Fatalf("expected to pull URL from X-GitLab-* header, got %#v", refs)
	}
}

func TestIsIgnored(t *testing.T) {
	cfg.IgnoreLabels = []string{"wip", "Draft", "  needs review  "}
	cases := map[string]bool{
		"wip":          true,
		"WIP":          true, // case-insensitive
		"draft":        true, // case-insensitive
		"needs review": true, // whitespace trimmed on key
		"bug":          false,
		"":             false,
	}
	for in, want := range cases {
		if got := isIgnored(in); got != want {
			t.Errorf("isIgnored(%q) = %v, want %v", in, got, want)
		}
	}
	cfg.IgnoreLabels = nil
}

func TestIsIgnoredPrefixes(t *testing.T) {
	cfg.IgnorePrefixes = []string{"Redmine::", " tmp/ "}
	cases := map[string]bool{
		"redmine::5907": true,
		"REDMINE::1":    true,
		"redmine":       false, // prefix must match fully, bare name stays
		"tmp/scratch":   true,
		"bug":           false,
	}
	for in, want := range cases {
		if got := isIgnored(in); got != want {
			t.Errorf("isIgnored(%q) = %v, want %v", in, got, want)
		}
	}
	cfg.IgnorePrefixes = nil
}

func TestRenameLabel(t *testing.T) {
	cfg.Rename = map[string]string{
		"my-super-label": "msl",
		"Needs Review":   "review", // mixed case key
	}
	cases := map[string]string{
		"my-super-label": "msl",
		"My-Super-Label": "msl", // case-insensitive lookup
		"needs review":   "review",
		"bug":            "bug", // no mapping → unchanged
		"":               "",
	}
	for in, want := range cases {
		if got := renameLabel(in); got != want {
			t.Errorf("renameLabel(%q) = %q, want %q", in, got, want)
		}
	}
	cfg.Rename = nil
}

func TestStripPrefix(t *testing.T) {
	cfg.StripPrefixes = []string{"backport-", "Type::", " release/ "} // mixed case + whitespace
	cases := map[string]string{
		"backport-v1.0":   "v1.0",
		"Backport-v2.0":   "v2.0",    // case-insensitive prefix
		"type::bug":       "bug",     // case-insensitive against mixed-case config
		"release/2025-05": "2025-05", // whitespace in config key trimmed
		"unrelated":       "unrelated",
		"backport-":       "backport-", // would strip to empty → preserve original
	}
	for in, want := range cases {
		if got := stripPrefix(in); got != want {
			t.Errorf("stripPrefix(%q) = %q, want %q", in, got, want)
		}
	}
	// First matching prefix wins — both "a" and "ab-" match "ab-thing"; "a" is listed first.
	cfg.StripPrefixes = []string{"a", "ab-"}
	if got := stripPrefix("ab-thing"); got != "b-thing" {
		t.Errorf("first-match: got %q, want %q", got, "b-thing")
	}
	cfg.StripPrefixes = nil
}

func TestFlagName(t *testing.T) {
	cfg.LabelPrefix = "gitlab/"
	if got := flagName("bug"); got != "gitlab/bug" {
		t.Errorf("got %q want %q", got, "gitlab/bug")
	}
	if got := flagName("needs review"); got != "gitlab/needs_review" {
		t.Errorf("spaces should become underscores, got %q", got)
	}
	cfg.LabelPrefix = ""
	if got := flagName("bug"); got != "bug" {
		t.Errorf("empty prefix: got %q want %q", got, "bug")
	}
}

func TestFetchLabelsHitsExpectedURLAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("PRIVATE-TOKEN") != "test-token" {
			t.Errorf("expected PRIVATE-TOKEN header, got %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		// PathEscape encodes "group/project" → "pm%2Fkubernetes" but the net/http
		// router decodes path segments; verify by inspecting r.URL.EscapedPath.
		wantPath := "/api/v4/projects/group%2Fproject/issues/108"
		if r.URL.EscapedPath() != wantPath {
			t.Errorf("path = %q want %q", r.URL.EscapedPath(), wantPath)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []string{"bug", "needs review"}})
	}))
	defer srv.Close()

	cfg = pluginConfig{
		BaseURL:     srv.URL,
		Token:       "test-token",
		LabelPrefix: "gitlab/",
		CacheTTL:    1 * time.Minute,
		HTTPTimeout: 2 * time.Second,
	}
	tokenInUse = cfg.Token
	httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	// reset cache
	cacheMu.Lock()
	cache = map[string]cachedLabels{}
	cacheMu.Unlock()

	ref := issueRef{Host: "gitlab.example", Project: "group/project", Kind: "issues", IID: "108"}
	got, tb, _, err := fetchLabels(ref)
	if err != nil {
		t.Fatalf("fetchLabels error: %v", err)
	}
	if tb != "" {
		t.Errorf("issues should have empty target_branch, got %q", tb)
	}
	want := []string{"bug", "needs review"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v want %v", got, want)
	}

	// Second call must hit the in-memory cache, not the server.
	if _, _, _, err := fetchLabels(ref); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 API hit (caching), got %d", hits)
	}
}

func TestFetchLabelsMergeRequestEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v4/projects/group%2Fproject/merge_requests/55"
		if r.URL.EscapedPath() != wantPath {
			t.Errorf("path = %q want %q", r.URL.EscapedPath(), wantPath)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []string{"review"}, "target_branch": "main", "state": "merged"})
	}))
	defer srv.Close()
	cfg = pluginConfig{BaseURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second}
	httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	cacheMu.Lock()
	cache = map[string]cachedLabels{}
	cacheMu.Unlock()

	got, tb, state, err := fetchLabels(issueRef{Host: "gitlab.example", Project: "group/project", Kind: "merge_requests", IID: "55"})
	if err != nil {
		t.Fatalf("fetchLabels: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"review"}) {
		t.Fatalf("labels = %v want [review]", got)
	}
	if tb != "main" {
		t.Errorf("target_branch = %q want %q", tb, "main")
	}
	if state != "merged" {
		t.Errorf("state = %q want %q", state, "merged")
	}
}

func TestFetchLabelsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg = pluginConfig{BaseURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second}
	httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	cacheMu.Lock()
	cache = map[string]cachedLabels{}
	cacheMu.Unlock()

	_, _, _, err := fetchLabels(issueRef{Host: "x", Project: "a/b", Kind: "issues", IID: "1"})
	if err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestStateActionKey(t *testing.T) {
	cases := []struct {
		kind, state, want string
	}{
		{"issues", "closed", "issue-closed"},
		{"issues", "opened", ""},
		{"merge_requests", "merged", "mr-merged"},
		{"merge_requests", "closed", "mr-closed"},
		{"merge_requests", "opened", ""},
		{"merge_requests", "locked", ""},
	}
	for _, c := range cases {
		if got := stateActionKey(c.kind, c.state); got != c.want {
			t.Errorf("stateActionKey(%q,%q) = %q want %q", c.kind, c.state, got, c.want)
		}
	}
}

func TestParseStateAction(t *testing.T) {
	cases := []struct {
		spec     string
		wantType string
		wantVal  string
		wantOK   bool
	}{
		{"move-to=>Junk", "move", "Junk", true},
		{"move => Archive ", "move", "Archive", true}, // alias + whitespace tolerance
		{"delete", "delete", "", true},
		{"mark-read", "mark_as_read", "", true},
		{"mark_as_read", "mark_as_read", "", true},
		{"move-to=>", "", "", false}, // missing mailbox
		{"", "", "", false},
		{"frobnicate", "", "", false},
	}
	for _, c := range cases {
		got, ok := parseStateAction(c.spec)
		if ok != c.wantOK || got.Type != c.wantType || got.Value != c.wantVal {
			t.Errorf("parseStateAction(%q) = %+v,%v want {%q %q},%v", c.spec, got, ok, c.wantType, c.wantVal, c.wantOK)
		}
	}
}

func TestStateActionForConfigMatching(t *testing.T) {
	cfg.StateActions = map[string]string{
		"MR_Merged":    "move-to=>Archive", // mixed case + underscore key
		"issue-closed": "delete",
	}
	defer func() { cfg.StateActions = nil }()

	if spec, ok := stateActionFor("merge_requests", "merged"); !ok || spec != "move-to=>Archive" {
		t.Errorf("mr-merged lookup = %q,%v", spec, ok)
	}
	if spec, ok := stateActionFor("issues", "closed"); !ok || spec != "delete" {
		t.Errorf("issue-closed lookup = %q,%v", spec, ok)
	}
	if _, ok := stateActionFor("merge_requests", "closed"); ok {
		t.Error("mr-closed should not match: not configured")
	}
	if _, ok := stateActionFor("issues", "opened"); ok {
		t.Error("opened issue should never trigger")
	}
}

func TestParseRetryAfter(t *testing.T) {
	mk := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	if d := parseRetryAfter(mk("Retry-After", "30")); d != 30*time.Second {
		t.Errorf("delta-seconds: got %s want 30s", d)
	}
	// RateLimit-Reset epoch ~20s out (allow slack for clock/rounding).
	reset := time.Now().Add(20 * time.Second).Unix()
	if d := parseRetryAfter(mk("RateLimit-Reset", strconv.FormatInt(reset, 10))); d < 15*time.Second || d > 25*time.Second {
		t.Errorf("RateLimit-Reset epoch: got %s want ~20s", d)
	}
	// No headers → default.
	if d := parseRetryAfter(mk()); d != defaultBackoff {
		t.Errorf("no headers: got %s want %s", d, defaultBackoff)
	}
	// Retry-After wins over RateLimit-Reset when both present.
	if d := parseRetryAfter(mk("Retry-After", "5", "RateLimit-Reset", "9999999999")); d != 5*time.Second {
		t.Errorf("Retry-After precedence: got %s want 5s", d)
	}
}

func TestEnterBackoffClampsAndNeverShortens(t *testing.T) {
	defer func() {
		rateLimitMu.Lock()
		rateLimitedUntil = time.Time{}
		rateLimitMu.Unlock()
	}()

	enterBackoff(time.Hour) // beyond maxBackoff → clamped
	if d := rateLimitRemaining(); d <= 0 || d > maxBackoff+time.Second {
		t.Errorf("clamp: remaining %s, want ≤ %s", d, maxBackoff)
	}
	long := rateLimitRemaining()
	enterBackoff(time.Second) // shorter request must not shorten cooldown
	if d := rateLimitRemaining(); d < long-time.Second {
		t.Errorf("shorten guard: remaining dropped to %s from %s", d, long)
	}
}

func TestFetchLabelsShortCircuitsWhenRateLimited(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []string{"x"}})
	}))
	defer srv.Close()
	cfg = pluginConfig{BaseURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second}
	httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	cacheMu.Lock()
	cache = map[string]cachedLabels{}
	cacheMu.Unlock()
	enterBackoff(time.Minute)
	defer func() {
		rateLimitMu.Lock()
		rateLimitedUntil = time.Time{}
		rateLimitMu.Unlock()
	}()

	_, _, _, err := fetchLabels(issueRef{Host: "h", Project: "a/b", Kind: "issues", IID: "1"})
	if err == nil {
		t.Fatal("expected backoff error while rate-limited")
	}
	if hits != 0 {
		t.Errorf("expected no network call during backoff, got %d", hits)
	}
}

func TestFetchLabels429ArmsBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cfg = pluginConfig{BaseURL: srv.URL, CacheTTL: time.Minute, HTTPTimeout: time.Second}
	httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	cacheMu.Lock()
	cache = map[string]cachedLabels{}
	cacheMu.Unlock()
	rateLimitMu.Lock()
	rateLimitedUntil = time.Time{}
	rateLimitMu.Unlock()
	defer func() {
		rateLimitMu.Lock()
		rateLimitedUntil = time.Time{}
		rateLimitMu.Unlock()
	}()

	if _, _, _, err := fetchLabels(issueRef{Host: "h", Project: "a/b", Kind: "issues", IID: "1"}); err == nil {
		t.Fatal("expected error on 429")
	}
	if d := rateLimitRemaining(); d < 35*time.Second || d > 42*time.Second {
		t.Errorf("backoff after 429: remaining %s, want ~42s", d)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
