# ecr-prime

Trigger Amazon ECR **create-on-push** for a not-yet-existing repository by issuing the one
write standard OCI clients never do first — then cancel it. A single static binary, no
runtime deps beyond IRSA / Pod Identity / the AWS default credential chain.

## Why

Cross-account, a repo that doesn't exist yet has **no repository policy**, so any *read*
(the manifest/blob `HEAD` that `oras` and `docker` issue before pushing) returns
`403` and the client aborts — before any write. ECR create-on-push only fires on a **write**,
and only a *push-triggered* create applies the repository-creation-template policy (an
explicit `CreateRepository` API call does **not**). `ecr-prime` opens — then immediately
cancels — a blob upload (`POST /v2/<repo>/blobs/uploads/`). That write makes ECR create the
repo and stamp the template policy. A normal `oras`/`docker push` then succeeds (its
`HEAD` now returns `404` and it proceeds).

## Prerequisites

- AWS credentials resolvable via the **default chain** (IRSA / EKS Pod Identity / env / profile).
- The identity needs `ecr:GetAuthorizationToken` plus authorization to **create + push** in the
  target registry — i.e. a `CREATE_ON_PUSH` repository-creation-template covers the repo prefix
  and the registry's permissions/repo policy grant this principal (cross-account: granted by the
  *registry* account). A non-`202` means that authorization is missing.
- Run `ecr-prime` **once before the first push** to a new repo.

## Usage

Give the registry **account + region** and just the repo (recommended):

```sh
ecr-prime --account 123456789012 --region eu-west-1 my-namespace/my-app

# then push as usual — the repo now exists, so the client's HEAD returns 404 and proceeds
oras push 123456789012.dkr.ecr.eu-west-1.amazonaws.com/my-namespace/my-app:v1 file:type
```

Or pass a full registry host (region is parsed from it, override with `-region`/`$AWS_REGION`):

```sh
ecr-prime 123456789012.dkr.ecr.eu-west-1.amazonaws.com my-namespace/my-app
ecr-prime 123456789012.dkr.ecr.eu-west-1.amazonaws.com/my-namespace/my-app:v1   # tag ignored
```

The target host is always validated against the ECR endpoint shape, so the auth token is only
ever sent to a real `*.dkr.ecr.*.amazonaws.com[.cn]` host.

| Flag | Default | Meaning |
|---|---|---|
| `-account` | — | registry AWS account ID (12 digits); when set, pass only `<repo>` |
| `-region` | `$AWS_REGION` | AWS region (required with `-account`; else parsed from the host) |
| `-fips` | `false` | use the `ecr-fips` endpoint |
| `-partition` | `aws` | `aws` \| `aws-cn` \| `aws-us-gov` |
| `-keep` | `false` | keep the throwaway upload session instead of cancelling it |
| `-timeout` | `30s` | overall HTTP timeout |
| `-log-level` | `info` (`$LOG_LEVEL`) | `debug`/`info`/`warn`/`error` |
| `-version` | — | print version and exit |

**Exit codes:** `0` primed (repo created); `1` permanent failure — denied / no
`CREATE_ON_PUSH` template / out of scope (a `4xx`; also the least-privilege signal);
`2` usage; `3` transient failure after retries (throttling / `5xx` / network) — safe to
retry the job. Transient statuses (`429`/`5xx`) and network errors are retried internally
with exponential backoff + jitter before `3` is returned; `4xx` is never retried.

Logs: console when stderr is a TTY, JSON otherwise (ISO8601 timestamps).

## Build

```sh
make build      # CGO_ENABLED=0, static, version injected via -ldflags -X
```

## Install into oci-toolbox

Add to the toolbox `Dockerfile` build stage (alongside the other `go install` tools):

```dockerfile
RUN go install github.com/babs/ecr-prime@latest
```

then add `/go/bin/ecr-prime` to the `cp … /dist` list.
