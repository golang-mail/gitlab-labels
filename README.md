# gitlab-labels

Scans incoming mail for GitLab issue **and merge request** links, calls the GitLab API to fetch each one's labels, and applies them to the message — as go-mail UI labels by default, or as IMAP keywords when configured. The labels become visible in the UI and (with `local` storage) matchable by downstream filters. Optionally, it can also **act** on a message once its issue/MR reaches a terminal state (e.g. archive a notification when its MR merges).

Examples:
- Issue link `https://gitlab.example.com/group/project/-/issues/108#note_341321` → labels `gitlab/bug`, `gitlab/needs_review`
- MR link `https://gitlab.example.com/group/project/-/merge_requests/55` → labels `gitlab/review`, `gitlab/backend`

If a single email references both an issue and an MR (common in notification mail that links from one to the other), labels from both are merged and deduped.

## Install

```bash
go install github.com/golang-mail/gitlab-labels@latest
```

## How it works

1. Regex finds every `https?://<host>/<project-path>/-/(issues|merge_requests)/<iid>` in the body and any `X-GitLab-*` / `List-Id` header.
2. URLs are filtered by `config.hosts` (or any host if empty), deduped (by host+project+kind+iid, so issue #10 and MR !10 are tracked separately), and capped at `max_issues`.
3. For each reference, the plugin calls `GET {base_url}/api/v4/projects/{url-encoded-project}/{issues|merge_requests}/{iid}` with `PRIVATE-TOKEN`, reading the ref's `labels`, `target_branch` (MRs), and `state`.
4. Each kept label runs through the pipeline **ignore → rename → strip-prefix → prefix**, then becomes one action: a `label` action (default) or a `flag` action when `label_storage: imap`. Spaces are replaced with `_` so the value is a valid IMAP keyword.
5. If `state_actions` is configured and a ref is in a matching terminal state, the corresponding action (move/delete/mark-read) is emitted too — see [State-triggered actions](#state-triggered-actions).
6. Lookups are cached per `(host, project, iid)` for `cache_ttl` to avoid hammering GitLab on busy mailboxes. The manifest declares `cacheable: false` (the plugin owns its own TTL cache) and `rerunnable: true`, so re-filtering picks up label/state changes.

## Register

```yaml
plugins:
  - name: gitlab-labels
    command: ["/path/to/gitlab-labels"]
    transport: socket     # default; the plugin listens on $GO_MAIL_PLUGIN_SOCKET
    timeout: 8s           # host evaluate timeout; lookups run concurrently, so http_timeout + headroom is enough
    config:
      hosts:
        - gitlab.example.com
      # base_url: https://gitlab.example.com   # only override if API host differs
      token: glpat-xxxxxxxxxxxxxxxxxxxx          # or set $GITLAB_TOKEN
      label_prefix: "gitlab/"
      label_storage: local   # "local" (UI labels, default) or "imap" (IMAP keywords)
      max_issues: 5
      cache_ttl: 10m
      http_timeout: 4s
      verbose: false
      # Auto-file mail once its issue/MR reaches a terminal state (optional):
      state_actions:
        issue-closed: move-to=>Junk
        mr-merged:    move-to=>Archive
        mr-closed:    delete
```

Auth precedence: `config.token` wins; if unset, the plugin reads `$GITLAB_TOKEN` from its environment. With no token, public issues still work but private ones return an error and are skipped (set `verbose: true` to log).

### Config reference

| Key | Default | Purpose |
|---|---|---|
| `hosts` | (any) | Allowed GitLab hosts. Empty matches any host whose URL looks like a GitLab issue/MR. |
| `base_url` | `https://<host-from-url>` | Override the API host when emails reference a public name but the API lives elsewhere. |
| `token` | `$GITLAB_TOKEN` | GitLab personal access token. |
| `label_prefix` | `gitlab/` | Prepended to each label. Set to `""` for the raw label. |
| `label_storage` | `local` | `local` → `label` actions (go-mail UI labels); `imap` → `flag` actions (server-side IMAP keywords). |
| `max_issues` | `5` | Caps distinct refs resolved per email. |
| `cache_ttl` | `10m` | In-process lookup cache lifetime. |
| `http_timeout` | `4s` | Per-request timeout. |
| `verbose` | `false` | Log each lookup, rejected hosts, and ignored labels. |
| `rescan_interval` | (off) | e.g. `1h` — asks go-mail to periodically re-feed in-scope mail so new labels/states are picked up. |
| `ignore_labels` | — | Raw GitLab label names to drop (case-insensitive), e.g. `["wip", "draft"]`. |
| `ignore_prefixes` | — | Drop any raw label starting with one of these prefixes (case-insensitive), e.g. `["redmine::"]` for scoped labels. |
| `rename` | — | Map of GitLab label → output name (case-insensitive keys), applied before `label_prefix`. |
| `strip_prefixes` | — | Leading prefixes to drop from a label (case-insensitive, first match wins), e.g. `["backport-"]`. |
| `add_target_branch` | `false` | Also emit an MR's target branch as a label (runs through the same pipeline). |
| `add_kind_label` | `false` | Emit `mr` or `issue` as a label depending on the ref kind. |
| `state_actions` | — | Map of terminal state → action. See below. |

## Use it from filters

```yaml
filters:
  - name: tag-gitlab-issues
    accounts: ["work"]
    mailboxes: ["INBOX"]
    conditions:
      - field: plugin:gitlab-labels
        operator: matches
        value: ""
    actions: []   # labels/flags come from the plugin verdict
```

With `label_storage: local` (the default), you can chain follow-on filters that match on the applied label via the `label` field:

```yaml
  - name: route-gitlab-bugs
    accounts: ["work"]
    mailboxes: ["INBOX"]
    conditions:
      - field: label
        operator: contains
        value: "gitlab/bug"
    actions:
      - type: move
        value: "Bugs"
```

> **Note:** the filter engine matches labels via the `label` condition, which reads go-mail's UI labels — i.e. `label_storage: local`. With `label_storage: imap`, labels are written as IMAP keywords for external clients (Thunderbird, Apple Mail) but are **not** matchable by go-mail filter conditions. Use `local` if you want to chain filters; use `imap` if you want server-side keywords. (`status` only matches read/unread, not labels.)

## State-triggered actions

Beyond tagging, the plugin can act on mail once its referenced issue/MR is **done** — e.g. archive a notification once its MR merges. Configure `state_actions` as a map from a terminal state to an action:

```yaml
config:
  state_actions:
    issue-closed: move-to=>Junk      # issue state == "closed"
    mr-merged:    move-to=>Archive   # MR state == "merged"
    mr-closed:    delete             # MR state == "closed" (closed without merging)
```

- **Keys**: `issue-closed`, `mr-merged`, `mr-closed`. Matched case-insensitively, and `_` is treated as `-` (so `mr_merged` works too). Only these terminal states trigger — `opened` and `locked` refs never fire.
- **Action syntax**: `move-to=>Mailbox` (alias `move`), `delete`, `mark-read` (alias `mark_as_read`). The `=>` argument is the target mailbox for `move-to`; the other verbs take no argument. Unknown/empty specs are logged and skipped.
- State actions fire **in addition to** label/flag actions, and **even when the ref carries no labels** — so a merged MR notification still gets filed.
- If one email references several refs in the same state (e.g. three merged MRs), the action is emitted **once** (deduped by type + value).

Because the plugin emits real `move`/`delete` actions here, the state you observe is bounded by `cache_ttl`: a just-merged MR won't trigger until its cached entry expires and the email is re-evaluated. Lower `cache_ttl` for faster reaction.

## Rate limiting & concurrency

- **Concurrent lookups:** refs in one email are resolved in parallel (bounded), so total evaluate time is roughly one `http_timeout` rather than `max_issues × http_timeout`. This keeps a multi-link email from exceeding the host's per-plugin `timeout` (which would tear the plugin down and, after `max_restarts`, disable it). A host `timeout` of `http_timeout + ~2s` is plenty.
- **GitLab backoff:** on `429 Too Many Requests` or `503`, the plugin honors `Retry-After` (or `RateLimit-Reset`) and arms a **process-wide cooldown** — while it's active, lookups short-circuit without hitting GitLab, so evaluates stay fast and GitLab stops being hammered until the window resets. The cooldown is clamped to 15m. Labels simply aren't applied for refs looked up during a cooldown; a later rescan picks them up.

## Notes

- The plugin declares `run_last: false` — it primarily tags, so it runs alongside other matchers without deferring them. If you rely on `state_actions` to move/delete and want cleanup filters to settle the mailbox first, set `run_last: true` in the manifest.
- The regex requires the literal `/-/issues/` or `/-/merge_requests/` segment, which is GitLab's routing convention. Snippets, commits, and pipelines are out of scope.
- Subgroups are supported: `group/sub/proj/-/issues/N` (or `/-/merge_requests/N`) resolves to project `group/sub/proj`.
- IMAP keywords cannot contain spaces — the plugin replaces spaces in label names with `_`. If your labels contain other characters your IMAP server rejects (`*`, `%`, etc.), strip them in GitLab or extend `flagName`.
