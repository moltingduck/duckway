# GitHub Mintable Token Permissions

This document defines per-client authorization for GitHub App installation tokens minted by Duckway. The policy is attached to a placeholder-key assignment and is enforced before a request reaches GitHub.

## Security model

Authorization has three independent layers:

1. The GitHub App installation limits which repositories Duckway can access.
2. The GitHub App manifest limits the maximum permissions that GitHub can grant.
3. The assignment policy limits what one Duckway client may do with one placeholder key.

Duckway must enforce the assignment policy locally even when GitHub would accept a broader installation token. Minted tokens request only the permissions needed by the current request. An empty or invalid policy fails closed.

## Policy format

New policies use version `2` and provider `github`:

```json
{
  "version": "2",
  "provider": "github",
  "repositories": {
    "owner/repository": {
      "capabilities": {
        "clone": true,
        "push": false,
        "issues_read": true,
        "issues_write": false,
        "pull_requests_read": true,
        "pull_requests_write": false,
        "releases_read": true,
        "releases_write": false,
        "actions_read": true,
        "workflow_dispatch": true,
        "workflow_rerun": false,
        "workflow_cancel": false
      },
      "workflow_allowlist": ["ci.yml"],
      "ref_allowlist": ["main", "release/*"],
      "environment_allowlist": ["staging"]
    }
  }
}
```

Repository names are case-insensitive and must use the exact `owner/repository` form. Unknown fields, unknown capabilities, empty repository maps, and policies with no enabled capability are rejected.

Write capabilities require their corresponding read capability. `push` requires `clone`. Workflow mutation capabilities require `actions_read`. `workflow_dispatch` also requires non-empty workflow and ref allowlists.

## Capability mapping

| Capability | Allowed GitHub operations | Minted permission |
| --- | --- | --- |
| `clone` | Git smart HTTP discovery and upload-pack | `contents:read` |
| `push` | Git smart HTTP discovery and receive-pack | `contents:write` |
| `issues_read` | Read issues and issue comments | `issues:read` |
| `issues_write` | Create or modify issues and comments | `issues:write` |
| `pull_requests_read` | Read pull requests and reviews | `pull_requests:read` |
| `pull_requests_write` | Create or modify pull requests and reviews | `pull_requests:write` |
| `releases_read` | Read releases and release assets | `contents:read` |
| `releases_write` | Create, modify, or delete releases and assets | `contents:write` |
| `actions_read` | Read workflows, runs, jobs, and artifacts | `actions:read` |
| `workflow_dispatch` | Dispatch an allowed workflow on an allowed ref | `actions:write` |
| `workflow_rerun` | Re-run an Actions run | `actions:write` |
| `workflow_cancel` | Cancel an Actions run | `actions:write` |

Repository administration, secrets, variables, hooks, deployments, environment administration, package administration, organization administration, and GitHub App administration are not granted by this policy version.

`environment_allowlist` constrains the `inputs.environment` dispatch input when present. It does not grant permission to create or modify GitHub environments. A workflow that derives an environment by another mechanism remains responsible for its own GitHub environment protection rules.

## Presets

The UI exposes presets as editing conveniences. Presets are expanded into an explicit policy before storage.

| Preset | Capabilities |
| --- | --- |
| Read only | `clone`, issue/PR/release read, and `actions_read` |
| Developer | Read only plus `push`, issue write, and PR write |
| CI/CD operator | Read only plus workflow dispatch, rerun, and cancel |
| Custom | Individually selected capabilities and constraints |

Changing a preset never changes the GitHub App installation itself.

The assignment UI treats the installation token's returned `permissions` as a hard upper bound. Capabilities and presets that require a missing or weaker GitHub App permission are disabled. Hovering a disabled capability explains the required provider permission. Existing out-of-range capability values remain visible but locked during editing so unrelated changes do not silently rewrite the stored policy.

## Enforcement flow

1. Resolve the client placeholder assignment.
2. Reject missing, malformed, or non-GitHub policy data.
3. Match the repository, method, path, query, and constrained request fields.
4. Derive the minimum GitHub installation-token permission for that request.
5. Mint or reuse a token whose cache key includes the App credential fingerprint, repository, requested GitHub permission, and assignment-policy digest.
6. Proxy the request and record the client, key, method, repository-scoped path, and result in the existing request audit data. The capability is recoverable from the method/path mapping above.

The raw minted token is never included in logs or API responses.

## Compatibility

Version `1` endpoint ACLs remain readable and enforceable. Existing assignments are not rewritten automatically. Saving an existing GitHub assignment through the new editor converts it to version `2`.

Unknown future versions fail closed. Changing either the assignment policy or GitHub App private key creates a distinct token-cache identity, so stale tokens are not reused.

## Testing

Unit tests cover schema rejection, capability dependencies, endpoint compilation, request-body constraints, least-privilege token requests, cache invalidation, and v1 compatibility. Browser tests cover presets, custom controls, repository selection, and edit round trips.

Live tests use the git-ignored credentials described in the live-credential section of `docs/developer-guide.md`. They skip when credentials are absent and keep refresh, clone, and GitHub API capability checks independent so one unavailable operation does not hide another result.
